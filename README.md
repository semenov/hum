<p align="center">
  <img src="assets/logo.svg" alt="hum" width="300" height="98">
</p>

<p align="center">
  Run a good LLM on your Mac. One command, nothing to configure.
</p>

<p align="center">
  <img src="assets/demo.gif" alt="Installing hum, starting it, and asking it a question" width="900">
</p>

## Get started

```sh
brew tap semenov/hum https://github.com/semenov/hum
brew trust semenov/hum
brew install --HEAD hum
```

`brew trust` is a one-off. Homebrew refuses to build a formula from a tap it has
not been told to trust, which is fair — a formula is a script that runs on your
machine.

Then:

```sh
hum start
```

That is it. The first time, it downloads the model — 20 GB, once, with a
progress bar and something to read while you wait. Every time after that it
takes a few seconds.

Now you have an OpenAI-compatible server on **http://127.0.0.1:4242/v1**. No
account, no API key, no per-token bill, and nothing you type leaves the machine.

## Try it without any setup

```sh
hum chat
```

```
  ● Chatting with Qwen3.6 35B-A3B

  you › name three roman emperors

  hum › Augustus, Trajan and Marcus Aurelius.

  246 tokens · 92 tok/s · thought for 0.3s
```

Or one question at a time, which is handy in a pipe:

```sh
hum ask "why is the sky blue"
cat error.log | hum ask "what is failing here?"
```

## Plug it into your tools

Point anything that speaks the OpenAI API at `http://127.0.0.1:4242/v1`. Most
tools want three things:

| | |
|---|---|
| Base URL | `http://127.0.0.1:4242/v1` |
| API key | anything at all — it is not checked |
| Model | `hum` |

The model id is always `hum`, whatever weights are actually loaded, so nothing
you configure has to be edited when the model changes. `GET /v1/models` reports
the real name and path next to it. Any other id works too — the field is
ignored, because there is only one model to serve.

That covers OpenCode, Cursor, Continue, the `openai` Python and Node SDKs,
LangChain, and most of the rest. Three of them in full:

### OpenCode

Add a provider to `~/.config/opencode/opencode.json`:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "hum": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "hum (local)",
      "options": { "baseURL": "http://127.0.0.1:4242/v1" },
      "models": { "hum": { "name": "Qwen3.6 35B-A3B" } }
    }
  },
  "model": "hum/hum"
}
```

The last line makes it the default; drop it and pick `hum` from `/models`
inside OpenCode instead. If you already have a `provider` block, add `hum`
alongside what is there rather than replacing it. To try it once without
changing your default:

```sh
opencode run -m hum/hum "read note.txt and tell me what it contains"
```

Two things to expect. OpenCode sends its system prompt and the whole tool
schema on every turn — about 7,400 tokens before your message even starts — so
the first request of a session pauses to prefill, roughly 7 s at the 1,100
tok/s this machine manages. After that the prompt cache holds the prefix and
later turns prefill only what is new: 61 and 112 tokens on the two turns that
followed here. That is why the first question feels slow and the rest do not.

And a 35B model with 3B active parameters is a capable assistant but not a
frontier one. Checked against OpenCode 1.18.22: reading a file and reporting
its contents took 14 s across a glob, a read and the answer; fixing a one-line
bug took 22 s across a glob, a read and an edit, and the edit was right. It is
good at localised work like that, and it will struggle where a large model
would carry a long plan across many files.

### Node.js

The official SDK, pointed somewhere else:

```sh
npm install openai
```

```js
import OpenAI from "openai";

const hum = new OpenAI({
  baseURL: "http://127.0.0.1:4242/v1",
  apiKey: "unused", // required by the SDK, ignored by hum
});

const r = await hum.chat.completions.create({
  model: "hum",
  messages: [{ role: "user", content: "name three roman emperors" }],
});
console.log(r.choices[0].message.content);
```

Streaming is the same call with `stream: true`:

```js
const stream = await hum.chat.completions.create({
  model: "hum",
  messages: [{ role: "user", content: "count from 1 to 5" }],
  stream: true,
});
for await (const part of stream) {
  process.stdout.write(part.choices[0]?.delta?.content ?? "");
}
```

Tools work as they do upstream — send JSON Schema, get a validated call back:

```js
const r = await hum.chat.completions.create({
  model: "hum",
  messages: [{ role: "user", content: "what is the weather in Lisbon?" }],
  tools: [{
    type: "function",
    function: {
      name: "get_weather",
      description: "Current weather for a city",
      parameters: {
        type: "object",
        properties: { city: { type: "string" } },
        required: ["city"],
      },
    },
  }],
});
console.log(r.choices[0].message.tool_calls);
// [{ id: "...", type: "function",
//    function: { name: "get_weather", arguments: "{\"city\":\"Lisbon\"}" } }]
```

No dependency at all, if you would rather not have one:

```js
const r = await fetch("http://127.0.0.1:4242/v1/chat/completions", {
  method: "POST",
  headers: { "content-type": "application/json" },
  body: JSON.stringify({
    model: "hum",
    messages: [{ role: "user", content: "hello" }],
  }),
}).then((r) => r.json());
console.log(r.choices[0].message.content);
```

### Python

```sh
pip install openai
```

```python
from openai import OpenAI

hum = OpenAI(base_url="http://127.0.0.1:4242/v1", api_key="unused")

r = hum.chat.completions.create(
    model="hum",
    messages=[{"role": "user", "content": "name three roman emperors"}],
)
print(r.choices[0].message.content)
```

Streaming:

```python
for chunk in hum.chat.completions.create(
    model="hum",
    messages=[{"role": "user", "content": "count from 1 to 5"}],
    stream=True,
):
    print(chunk.choices[0].delta.content or "", end="", flush=True)
```

Tools:

```python
r = hum.chat.completions.create(
    model="hum",
    messages=[{"role": "user", "content": "what is the weather in Lisbon?"}],
    tools=[{
        "type": "function",
        "function": {
            "name": "get_weather",
            "description": "Current weather for a city",
            "parameters": {
                "type": "object",
                "properties": {"city": {"type": "string"}},
                "required": ["city"],
            },
        },
    }],
)
call = r.choices[0].message.tool_calls[0]
print(call.function.name, call.function.arguments)
# get_weather {"city":"Lisbon"}
```

Anything outside the OpenAI schema — such as [reasoning control](#reasoning-control)
— goes through `extra_body`, since the SDK validates what it sends:

```python
r = hum.chat.completions.create(
    model="hum",
    messages=[{"role": "user", "content": "17 * 23?"}],
    extra_body={"reasoning": {"effort": "none"}},
)
```

In JavaScript the same field goes straight in the request object; the SDK
passes through what it does not recognise.

### Two things that catch people out

**The thinking is a separate field.** `message.content` is the answer only;
the reasoning that produced it is in `message.reasoning` (and
`reasoning_content`, which is the same string under the name some clients
expect). The Python SDK does not model it, so read it from
`message.model_extra["reasoning"]`.

**`max_tokens` covers the thinking too.** Send a small one and a thinking model
can spend the whole budget reasoning and hand back an empty message. hum
defaults to 4096 for that reason. If you want short answers rather than a short
budget, turn the thinking off instead: `{"reasoning": {"effort": "none"}}`.

Calling from a browser needs `hum start --cors`, which is off by default
because any page you have open could otherwise reach the server.

## The commands

```
hum            this, with colours
hum start      start it (downloads the model the first time)
hum stop       stop it and free the memory
hum status     is it running, where, which model
hum chat       talk to it here
hum agent      the same, but it can read, write and run things
hum ask "…"    one question, one answer
hum run "…"    give the agent a task and let it finish
hum logs -f    watch what it is doing
hum model      which model it runs, where it lives, and how big it is
```

## If something is wrong

**It says this Mac cannot run it.** hum needs Apple Silicon — MLX has no other
target. On an Intel Mac, llama.cpp is the usual answer. It also needs 32 GB of
memory for the one model it ships; on a smaller Mac, `hum start --model
/path/to/any/mlx/model` runs whatever you point it at instead.

**The download stopped.** Run `hum start` again; it picks up where it left off.

**It will not start.** `hum logs -n 50` shows what the server and the Python
worker both said.

**You want the memory back.** `hum stop` unloads the model. `rm -rf ~/.hum` also
removes the downloaded weights and the settings.

## Requirements

An Apple Silicon Mac with **at least 32 GB** of unified memory. That is all —
Homebrew brings its own Python and installs `mlx-lm` into a virtualenv of its
own, so nothing lands in yours.

On a smaller Mac `hum start` stops and says so rather than downloading
something first. See [below](#the-model-is-chosen-for-you) for why there is no
fallback to a smaller model, and what to do instead.

---

## The model is chosen for you

There is nothing to configure, and nothing to choose. `hum` runs one model:

| model | download | on an M3 Max |
|---|---|---|
| Qwen3.6 35B-A3B, 4-bit | 20.4 GB | 89 tok/s |

That is the whole catalogue, and the reason for the 32 GB requirement — the
weights plus the KV cache have to stay under the wired limit, which macOS puts
at roughly 75% of RAM.

**Why no smaller model for a smaller Mac?** Because a tier list is a promise
that each entry is the best thing that fits, and honouring that means measuring
every one of them. A model picked by file size alone is a guess with a progress
bar in front of it. One model that has been benchmarked end to end is worth more
than five that have not, so a 16 GB Mac gets a clear refusal instead of a
20-minute download and a disappointment. Smaller tiers may come back once they
have earned their place.

Meanwhile, nothing stops you running your own: `hum start --model
/path/to/any/mlx/model` skips the memory check entirely.

`hum model` shows the pick and why. Weights land in `~/.hum/models/` and are
resumed rather than restarted if a download is interrupted. Delete the
directory and the next `hum start` fetches it again.

To override: `hum start --model /path/to/any/mlx/model`, or set
`HUM_MODEL_REPO` to any Hugging Face MLX repo.

It is also **faster than the alternatives**, measured rather than asserted.

## The port

4242. Unassigned by IANA, and easy to remember, which is the point — a port you
have to look up every time is a worse port than one that occasionally collides.
It stays clear of 1234 (LM Studio), 11434 (Ollama) and the usual 8080/8000/3000.

    hum start --addr 127.0.0.1:8080     # a different port
    hum start --addr 0.0.0.0:4242       # reachable from a VM or the network

It binds to localhost by default. There is no authentication of any kind — the
same as LM Studio and Ollama, both of which also bind localhost and ship no auth
— so opening it to the network means anyone who can route to the machine can use
the model. Fine at home, not in a cafe. When you do bind wide, `hum start` says
so and prints the address other machines should use.

Browsers are a separate door. By default no CORS headers are sent, so a page you
visit cannot reach the server even though it runs on your own machine. Building
a web app against it needs `--cors`, which lets *any* page reach it:

    hum start --cors

## Reasoning control

This model thinks before it answers, which is often what you want and sometimes
not — it can spend a thousand tokens deciding what 9.11 versus 9.9 means. All
three spellings the ecosystem uses are accepted:

```jsonc
{"reasoning": {"effort": "low"}}      // OpenRouter
{"reasoning": {"max_tokens": 200}}    // a hard budget
{"reasoning": {"exclude": true}}      // think, but do not return it
{"reasoning_effort": "none"}          // OpenAI spelling; none disables thinking
{"include_reasoning": false}          // legacy, same as exclude
```

There is no effort dial inside the model, so effort is expressed as how long it
may think: `minimal` 256 tokens, `low` 1024, `medium` 4096, `high` unbounded.
The budget is enforced rather than requested — once it is spent, every token
except `</think>` is masked out, so the model has to stop. `none` is different
again: the chat template emits an already-closed think block and the model never
reasons at all.

Measured on "which is larger, 9.11 or 9.9":

| | reasoning | total tokens |
|---|---|---|
| default | 2097 chars | 977 |
| `effort: minimal` | 888 chars | 526 |
| `reasoning.max_tokens: 200` | 678 chars | 531 |
| `effort: none` | none | 242 |

Reasoning comes back as both `reasoning_content` and `reasoning`, since clients
read one or the other.

## Agent

`hum agent` is the same chat with hands, and `hum run "task"` is the same agent
without a prompt, for scripts:

```sh
hum run "which file defines the prompt cache, and how big is it?"
cat error.log | hum ask "what is failing here?"
```

It has five tools: read a file, list a directory, search the tree, write a file,
run a shell command.

**What it is allowed to do is deliberately narrow by default.** Reading and
searching are confined to the directory you start it in. Writing needs
`--allow-write`, and running commands needs `--allow-shell`; interactively it
asks before each one instead.

That split is not decoration. During testing the agent was told to write outside
its directory: `write_file` refused, and the model then routed around it with
`cat ../../etc/hosts` and `printf > /tmp/...`. A shell cannot be confined to a
directory, so granting it is a separate, explicit act — and the confirmation
prompt says so.

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

## Context

The model's window is **262,144 tokens** and hum does not narrow it — there is
no cap, no truncation and no sliding window in the server. What runs out first
is memory, and not in the place you would expect.

Only 10 of the 40 layers keep a growing KV cache; the other 30 are
linear-attention layers whose state is a fixed size no matter how long the
conversation gets. That makes context unusually cheap here — about 35 KB per
token, measured, against several times that for a dense model of the same size.

Measured on a 36 GB M3 Max, where the wired-memory limit is around 27 GB:

| context | resident after | peak during prefill | prefill |
|---|---|---|---|
| — | 18.4 GB | — | — |
| 8k | 19.0 GB | 21.9 GB | 8 s |
| 32k | 19.9 GB | 23.7 GB | 32 s |
| 64k | 21.0 GB | 27.1 GB | 104 s |

The cache itself is not the problem: 64k of it costs 2.6 GB. The peak is. Each
2,048-token prefill chunk attends against everything before it, and that score
matrix is transient but large — 6 GB at 64k, growing linearly with context. At
64k the peak touches the wired limit on this machine, so **64k is the practical
ceiling on 36 GB**, not the 256k the model advertises. A 48 GB or 64 GB Mac
goes further.

Two other things worth knowing. Prefill slows as context grows — 1,011 tok/s at
16k, 616 tok/s at 64k — so a full 64k prompt is a 100-second wait before the
first token, though the prompt cache means you pay it once per conversation
rather than once per turn. And the cache holds four conversations by default
(`--cache-entries`), each with its own KV, so four long sessions cost four
times the memory. It is bounded by count, not by bytes.

## Limitations

- **One request at a time.** No continuous batching; see the benchmark note.
- Context is bounded by memory rather than by a setting: nothing stops you
  sending a prompt too large for the machine, and what you get is swapping
  rather than a clear error. See [Context](#context).
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

