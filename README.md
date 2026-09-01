# hum

A fast, zero-config OpenAI-compatible LLM server for Apple Silicon.

    brew install hum && hum

Named for the sound a Mac makes while it works — which, at sustained 92 tok/s
with a pinned CPU core and the GPU at 70% of memory bandwidth, it does.

## Name availability (checked)

    Homebrew formula `hum`   free      <- the one that matters: `brew install hum`
    binary `hum` in PATH     free
    npm scope `@hum`         free      <- use `@hum/cli`; bare `hum` is squatted
                                          (dead grunt wrapper, 150 dl/mo)
    PyPI `humd` / `hum-server`  free   <- bare `hum` taken (audio synth)
    GitHub org `hum`         taken     <- repo can live anywhere; only the
                                          formula name binds the command
    hum.sh / hum.dev         taken

Known cost: `hum` is a common word and does not surface in search — a query for
it returns generic results, the name is swallowed. Discovery has to come from
links and word of mouth rather than SEO. This was measured and accepted.


## Result (Qwen3.6-35B-A3B-4bit, M3 Max)

| scenario | hum | LM Studio |
|---|---|---|
| short-prompt decode | **92.0 tok/s** | 84.5 tok/s |
| 9k prompt, cold TTFT | **7622 ms** | 8115 ms |
| multi-turn warm TTFT | **217 ms** | 462 ms |
| alternating 2 chats TTFT | **146 ms** | 462 ms |
| concurrency C=4/C=8 aggregate | 90 tok/s | **163 / 165 tok/s** |

Wins every single-stream scenario; loses only on concurrency (no batching).
For reference: mlx-lm's direct library API is 92.0 tok/s (the ceiling) and
`mlx_lm.server` is 72.6 tok/s.

## Why it is faster

Go cannot make MLX faster — the compute is Metal kernels, identical in every
case. The gain comes from *not stealing CPU from the decode loop*.

On this model a decode step is ~11 ms and the process already spends ~0.87
cores on Python graph-building plus single-threaded Metal command encoding.
CPU is at ~87% of GPU time, so `step = max(GPU, CPU)` — any extra Python work
on the generation thread costs throughput almost 1:1. That is exactly what
`mlx_lm.server` does (stop-string trie, thinking-tag state machine,
detokenization, SSE JSON — all per token, all in Python).

So: the Python worker does *only* prefill + decode and writes raw token ids to
a pipe. Go does detokenization, stop sequences and SSE framing on other cores.

    [client] --SSE--> [Go: detok, stop, JSON] --pipe--> [Python: prefill + decode]

Detokenization is exact: the worker exports an id -> utf-8 byte table at
startup (applying the BPE byte decoder once), and Go concatenates bytes and
emits only complete runes. Verified **byte-identical** to mlx-lm's own
detokenizer over a 220-token temp=0 generation including Cyrillic.

## Run

    go build -o hum .
    hum start --model ~/models/Qwen3.6-35B-A3B-MLX-4bit

`start` daemonises, waits until the model is actually loaded, then returns —
so the next command can rely on the server being up. The model path is saved to
`~/.hum/config.json`, so afterwards a bare `hum start` is enough.

    hum start      start in the background and return
    hum stop       stop it (kills the process group, so the worker goes too)
    hum restart    stop then start
    hum status     running? where? which model? how long?
    hum logs -f    follow the log
    hum serve      run in the foreground, for debugging
    hum config     show the saved configuration

State lives in `~/.hum/`: `config.json`, `hum.pid`, `hum.log`.

## Tool calling

Implemented against the format in the model's own `chat_template.jinja`:

    <tool_call>
    <function=NAME>
    <parameter=KEY>
    VALUE
    </parameter>
    </function>
    </tool_call>

`tools.go` is a streaming state machine over that syntax. Points that mattered:

- **Argument typing.** The template renders string arguments verbatim and
  everything else through `tojson`, so decoding inverts that using the declared
  JSON-schema type: `days` comes back as `3`, not `"3"`.
- **Split tags.** Tags arrive split across tokens. The splitter withholds a
  short tail; `TestChunkingIsInvariant` feeds the same text in 1/2/3/5/7/13-byte
  chunks and asserts identical output.
- **Reasoning state is seeded from the prompt.** The template appends `<think>`
  to the generation prompt, so the model emits only the *closing* tag. The
  worker reports whether reasoning is open (event `K`); without this the whole
  chain of thought is misreported as `content`.
- **`arguments` normalisation.** OpenAI sends `arguments` as a JSON *string*,
  but the template does `arguments|items` and needs a mapping. The worker
  converts on the way in, otherwise the tool round-trip 500s.

### Constrained decoding

Parsing alone is best-effort: a malformed call is silently lost and nothing
stops the model naming a function that was never offered. So generation is
*constrained*, the same way LM Studio does it — an `llguidance` grammar over the
model's own syntax, armed the moment the model emits the `<tool_call>` token and
released when the matcher completes:

    start: WS "<function=" tool ">" WS parameter* "</function>" WS <[CLOSE_ID]>
    tool: "get_weather" | "get_time"          # only the offered names
    parameter: "<parameter=" PARAM_NAME ">" param_value WS

`grammar_test.py` verifies rejection deterministically, without depending on
model behaviour — an invented name is blocked mid-token (`token "_stock" doesn't
satisfy the grammar`), as is JSON-instead-of-XML and a missing `</function>`.

Cost: **5.8%**, and only when `tools` are present (91.4 -> 86.1 tok/s). Plain
chat is unaffected. Note 86.1 with tools is still above LM Studio's 82.7
without.

Verified end to end: call -> `tool_calls` + `finish_reason: "tool_calls"`,
result fed back as `role: "tool"`, model answers. Streaming emits
`delta.tool_calls`.

Also returns `usage` (`prompt_tokens` / `completion_tokens` / `total_tokens`).

## Limitations (real, not hypothetical)

- **One request at a time.** A mutex serialises generation; there is no
  continuous batching. Measured: hum 90 tok/s aggregate at C=4 and C=8 vs
  LM Studio's 163/165. This is single-stream-latency optimised.
- Byte-level BPE verified (Qwen); the SPM branch of the byte-table export is
  written but untested.
- `/v1/chat/completions` only (stream and non-stream) plus `/v1/models`.
- Not implemented: `tool_choice`, `response_format` / JSON-schema structured
  output, `logprobs`, `n`, `seed`, `frequency_penalty` / `presence_penalty`,
  LoRA.
- Prompt-cache reuse IS implemented (snapshot at the stable history boundary):
  ~9k-token chat history goes 10975 ms -> ~145 ms TTFT on follow-up turns.
  Caveat: on hybrid linear-attention models, reusing the cache changes the
  generated text slightly, because mlx-lm's chunked prefill is not invariant to
  chunk size on those models (dense models are bit-identical). See FINDINGS.md.
- No auth, no request limits. Do not expose this to a network.
