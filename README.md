<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/logo-dark.svg" width="300" height="98">
    <img src="assets/logo.svg" alt="hum" width="300" height="98">
  </picture>
</p>

<p align="center">
  A fast, zero-config local LLM server for Apple&nbsp;Silicon.<br>
  OpenAI-compatible. One command. No GUI.
</p>

---

```sh
hum start
```

```
first run: fetching Qwen3.6 35B-A3B (20.4 GB)
  ███████████████░░░░░░░░░░░░░░░░░░░  44%  9.0 GB / 20.4 GB  86 MB/s  eta 2m13s
  Somewhere in these weights is a number that means "cat". Nobody knows which one.

downloaded 20.4 GB in 4m01s
starting hum (pid 4909), loading ~/.hum/models/…-35B-A3B-MLX-4bit
ready on http://127.0.0.1:8090  (logs: ~/.hum/hum.log)
```

No flags, no model to pick, no account. Point OpenCode, your editor, or any
OpenAI SDK at `http://127.0.0.1:8090/v1` and it works.

## Why

Running a local model on a Mac today means either a GUI you have to click
through, or a stack you have to assemble yourself — and either way you still
have to guess which model is worth running. `hum` is one binary, one command,
and a server that gets out of the way.

## The model is chosen for you

There is nothing to configure. On first run `hum` looks at how much unified
memory the machine has and fetches the largest model that comfortably fits —
weights plus KV cache have to stay under the wired limit, which macOS puts at
roughly 75% of RAM.

| system memory | model | download |
|---|---|---|
| 32 GB+ | Qwen3.6 35B-A3B | 20.4 GB |
| 24 GB | Qwen3 14B | 8.3 GB |
| 16 GB | Qwen3 8B | 4.6 GB |
| 8 GB | Qwen3 4B | 2.3 GB |
| below | Qwen3 0.6B | 0.4 GB |

`hum model` shows the pick and why. Weights land in `~/.hum/models/` and are
resumed rather than restarted if a download is interrupted. Delete the
directory and the next `hum start` fetches it again.

To override: `hum start --model /path/to/any/mlx/model`, or set
`HUM_MODEL_REPO` to any Hugging Face MLX repo.

It is also **faster than the alternatives**, measured rather than asserted.

## Benchmarks

Qwen3.6-35B-A3B-4bit on an M3 Max (36 GB), single stream, identical prompts and
sampling, same HTTP client, run sequentially:

| | hum | LM Studio | mlx_lm.server |
|---|---|---|---|
| short-prompt decode | **92.2 tok/s** | 84.5 | 72.6 |
| 9k prompt, cold TTFT | **7.5 s** | 8.1 s | — |
| multi-turn warm TTFT | **219 ms** | 462 ms | — |
| alternating 2 chats, TTFT | **148 ms** | 462 ms | — |

For reference, calling mlx-lm as a library with no server at all gives
92.0 tok/s — so `hum` costs essentially nothing over the ceiling.

The last row is worth a look: LM Studio takes 462 ms whether or not you switch
conversations, while `hum` drops to 148 ms because several conversation
prefixes stay resident.

**Where it loses:** concurrency. There is no continuous batching, so several
simultaneous requests are served one at a time (~90 tok/s aggregate, against
LM Studio's ~165). `hum` optimises single-stream latency.

## How it works

Go can't make MLX faster — the compute is Metal kernels either way. The gain
comes from *not stealing CPU from the decode loop*.

A decode step on this model is ~11 ms, and the process already spends ~0.87 of a
core on graph building plus single-threaded Metal command encoding. CPU sits at
~87% of GPU time, so `step = max(GPU, CPU)`: any extra Python work on the
generation thread costs throughput almost 1:1. That is exactly what a
Python-side server does — stop-string trie, thinking-tag state machine,
detokenisation, SSE JSON, all per token.

So the split is:

```
[client] --SSE--> [Go: detok, stop, tools, JSON] --pipe--> [Python: prefill + decode]
```

The Python worker does *only* prefill and the decode loop, and writes raw token
ids to a pipe. Go does the rest, on other cores.

Detokenisation is exact: the worker exports an `id -> utf-8 bytes` table at
startup and Go emits only complete runes. Verified **byte-identical** to
mlx-lm's own detokeniser over a 220-token temp=0 generation including Cyrillic.

### Prompt caching

The KV cache is snapshotted at the *stable* boundary of the conversation — the
history without the generation prompt — because the template appends
`<|im_start|>assistant\n<think>` at the end, and next turn the assistant's real
reply sits at that offset instead. Snapshot the whole prompt and the prefix
never matches again.

Snapshots are kept in an LRU (4 conversations by default), so switching between
chats does not throw the cache away.

## Tool calling

`tools` are passed to the model's chat template, and the reply is parsed back
into OpenAI `tool_calls`. This model's native syntax is XML, not JSON:

```
<tool_call>
<function=get_weather>
<parameter=city>
Moscow
</parameter>
</function>
</tool_call>
```

Parameters are typed from the JSON schema you sent, so `{"days": 3}` comes back
as a number, not `"3"`.

Parsing alone is best-effort, so generation is also **constrained**: an
`llguidance` grammar is armed the moment the model emits `<tool_call>` and
released when the call is complete. An invalid call cannot be produced — the
model cannot name a function you did not offer, and cannot break the format.

```
[OK] valid call                     accepted=True
[OK] second offered tool            accepted=True
[OK] INVENTED function name         accepted=False   <- rejected mid-token
[OK] prefix of an offered name      accepted=False
[OK] JSON instead of XML            accepted=False
[OK] missing </function>            accepted=False
```

Cost: **5.8%**, and only when `tools` are present (91.4 -> 86.1 tok/s). Plain
chat is unaffected. Note 86.1 with the grammar armed is still above LM Studio's
82.7 without it.

Round-trip is verified end to end: call -> `tool_calls` +
`finish_reason: "tool_calls"`, result fed back as `role: "tool"`, model answers.

## CLI

Run `hum` with no arguments for a coloured overview of everything below.

```
hum start      start in the background, wait until the model is loaded, return
hum stop       stop it (signals the process group, so the worker dies too)
hum restart    stop then start
hum status     running? where? which model? how long?
hum logs -f    follow the log
hum model      show which model was picked for this machine
hum serve      run in the foreground, for debugging
hum config     show the saved configuration
```

`start` polls `/health` and only returns once the server actually answers, so a
script can call `hum start` and immediately send a request.

State lives in `~/.hum/`: `config.json`, `hum.pid`, `hum.log`.

## Install

Needs Go 1.24+, and a Python with `mlx-lm` and `llguidance` for the worker.

```sh
git clone https://github.com/semenov/hum && cd hum
go build -o hum .
pip install mlx-lm llguidance
hum start --python $(which python3)
```

## Limitations

- **One request at a time.** No continuous batching; see the benchmark note.
- Byte-level BPE detokenisation is verified on Qwen; the SPM path is written
  but untested.
- `/v1/chat/completions` and `/v1/models` only.
- Not implemented: `tool_choice`, JSON-schema structured output, `logprobs`,
  `n`, `seed`, `frequency_penalty` / `presence_penalty`, LoRA.
- No auth and no request limits — do not expose this to a network.
- Prompt-cache reuse is not bit-deterministic: resuming from a snapshot changes
  prefill chunk boundaries, which changes rounding, which can flip a token.
  Semantically equivalent, not byte-identical. This is inherent to prefix
  caching, not specific to `hum`.

## Development

```sh
go test ./...              # tool-call parser, including chunk-split invariance
python grammar_test.py     # grammar rejects invalid calls (deterministic)
go vet ./...
```

## License

MIT
