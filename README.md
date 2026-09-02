<p align="center">
  <img src="assets/logo.svg" alt="hum" width="300" height="98">
</p>

<p align="center">
  Run a good LLM on your Mac. One command, nothing to configure.
</p>

<p align="center">
  <img src="assets/demo.gif" alt="Installing hum, starting it, and asking it a question" width="900">
</p>

**[Get started](#get-started)** · **[Commands](#the-commands)** · **[Clients & SDKs](#plug-it-into-your-tools)** · **[From a coding agent](#using-it-from-a-coding-agent)** · **[API](#reasoning-control)** · **[Benchmarks](#benchmarks)** · **[Limitations](#limitations)**

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

## Requirements

An Apple Silicon Mac with **at least 32 GB** of unified memory. That is all —
Homebrew brings its own Python and installs `mlx-lm` into a virtualenv of its
own, so nothing lands in yours.

On a smaller Mac `hum start` stops and says so rather than downloading 20 GB
first. See [The model](#the-model).

## Talk to it in the terminal

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
      "models": {
        "hum": {
          "name": "Qwen3.6 35B-A3B",
          "limit": { "context": 32000, "output": 8000 }
        }
      }
    }
  },
  "model": "hum/hum"
}
```

**Do not leave out `limit`.** It is how OpenCode knows when to compact the
conversation, and for a provider it has never heard of there is no other way to
find out — the OpenAI API has no field for it, and hum will not refuse an
oversized prompt either. Without it the session grows until your Mac starts
swapping, which looks like the machine dying rather than a limit being reached.
32,000 is safe on any Mac hum runs on; on 36 GB or more you can raise it to
65536. See [Context](#context) for where those numbers come from.

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

## Using it from a coding agent

Not as the agent's brain — a 35B model will not out-think a frontier one. As
somewhere to send the work that eats context and tokens without needing
judgement: squashing a build log before your expensive model reads it, first-pass
descriptions of forty files, classification, drafts. It is free, unlimited, and
takes eight requests at once, which makes `xargs -P 8` the natural shape.
[**OFFLOADING.md**](OFFLOADING.md) has the rule, measured recipes, and a block
to paste into `CLAUDE.md` or `AGENTS.md`.

## Agent

`hum agent` is the same chat with hands, and `hum run "task"` is the same agent
without a prompt, for scripts:

```sh
hum run "which file defines the prompt cache, and how big is it?"
```

It has five tools: read a file, list a directory, search the tree, write a file,
run a shell command.

**What it is allowed to do is deliberately narrow by default.** Reading and
searching are confined to the directory you start it in. Writing needs
`--allow-write`, and running commands needs `--allow-shell`; interactively it
asks before each one instead.

The split matters: a shell cannot be confined to a directory. Asked to write
outside its directory, the model will route around a refused `write_file` with
`cat ../../etc/hosts` or `printf > /tmp/...`, so granting the shell is a
separate and explicit act, and the confirmation prompt says so.

## Access

It listens on `127.0.0.1:4242` and has no authentication of any kind, the same
as LM Studio and Ollama. Binding it wider means anyone who can reach the machine
can use the model — fine at home, not in a cafe. Browsers are a separate door:
no CORS headers are sent, so a page you visit cannot reach the server even
though it runs on your own machine.

```sh
hum start --addr 0.0.0.0:4242   # reachable from a VM or the network
hum start --addr 127.0.0.1:8080 # if 4242 is taken
hum start --cors                # let *any* web page reach it
```

## The model

There is nothing to configure, and nothing to choose. `hum` runs one model:

| model | download | on an M3 Max |
|---|---|---|
| Qwen3.6 35B-A3B, 4-bit | 20.4 GB | ~90 tok/s |

That is the whole catalogue, and the reason for the 32 GB requirement — the
weights plus the KV cache have to stay under the wired limit, which macOS puts
at roughly 75% of RAM.

There is no smaller tier for a smaller Mac, because a tier list is a promise
that each entry is the best thing that fits, and hum has only measured this one.

Weights land in `~/.hum/models/` and resume rather than restart if a download is
interrupted. To run something else — including on a Mac under 32 GB, which skips
the memory check:

```sh
hum start --model /path/to/any/mlx/model
HUM_MODEL_REPO=org/repo hum start        # any Hugging Face MLX repo
```

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

## Tool calling

Send `tools` as you would upstream and get `tool_calls` back, with
`finish_reason: "tool_calls"`; feed the result in as `role: "tool"` and the
model answers. There are worked examples in both SDKs
[above](#plug-it-into-your-tools).

Two things are better than plain parsing. Parameters are typed from the JSON
schema you sent, so `{"days": 3}` comes back as a number rather than `"3"`. And
generation is constrained by a grammar armed the moment the model starts a
call, so an invalid one cannot be produced at all — it cannot name a function
you did not offer, and it cannot break the format. That costs **5.8%** while
`tools` are present, and nothing otherwise.

## Structured output

`response_format` works the way it does upstream, and for the same reason it
does in tool calls: the schema is compiled to a grammar and the logits are
masked, so output that does not match the schema is not merely rejected, it is
never generated.

```python
schema = {
    "type": "object",
    "properties": {
        "city": {"type": "string"},
        "country": {"type": "string"},
        "population": {"type": "integer"},
    },
    "required": ["city", "country", "population"],
    "additionalProperties": False,
}

r = hum.chat.completions.create(
    model="hum",
    messages=[{"role": "user", "content": "Tell me about Lisbon."}],
    response_format={"type": "json_schema",
                     "json_schema": {"name": "city", "schema": schema}},
)
json.loads(r.choices[0].message.content)
# {'city': 'Lisbon', 'country': 'Portugal', 'population': 550000}
```

`{"type": "json_object"}` also works and asks only for an object. Both are
free: measured at 90.2 tok/s unconstrained against 90.4 with a schema, which is
to say the masking disappears into the noise of an 11 ms decode step.

**Bound your numeric fields.** With no `maximum`, one more digit is always
valid JSON, so no grammar may forbid it — llguidance cannot, Outlines cannot,
nothing will. A model that has talked itself into writing `5,450,000` finds the
comma illegal and emits zeros until it runs out of budget. Measured over eight
cities asking for `{"population": <integer>}`:

| | parsed |
|---|---|
| reasoning on, unbounded | 2 of 4 |
| reasoning off, unbounded | 7 of 8 |
| reasoning off, `"maximum": 50000000` | 8 of 8 |

Which is why a `response_format` request that says nothing about reasoning gets
none — it moves the odds a long way, though only the bound removes the failure.
Ask for reasoning explicitly and you get it, runaway integers included.

The grammar is armed only once the think block closes, so reasoning and a
required shape do compose when you want both.

Enums, nested objects, arrays with `minItems` and integer types are all
enforced, since llguidance compiles the schema rather than approximating it. If
you send `tools` and `response_format` together, the schema wins: asking for
both is contradictory, and a caller who wants a shape back wants it more than a
function call.

## Sampling

Beyond `temperature` and `top_p`:

| | |
|---|---|
| `frequency_penalty` | subtracts from a token's logit per time it has appeared |
| `presence_penalty` | subtracts once if it has appeared at all |
| `repetition_penalty` | divides the logit of anything in the last 20 tokens |
| `logit_bias` | `{"77916": -100}` — added straight to that token's logit |

`repetition_penalty` is not an OpenAI parameter, but every local runtime has it
and mlx-lm implements it, so it is here too. Unset means unset: a penalty you
do not send is not the same as sending zero, and hum passes the difference
through rather than flattening it.

Worth knowing that these fight the model's confidence rather than overruling
it. Asked for "banana ten times" at temperature 0, `frequency_penalty` of 1.5
and even 3 changes nothing, because the gap between banana and everything else
is enormous; at 8 the output finally breaks apart. `logit_bias` is the blunt
one — ban the token for " Lisbon" and the answer comes back "Lisboa".

They do **not** rescue a runaway inside a JSON schema. Penalising recent digits
makes the model choose different digits rather than close the number:
`231900000000` became `2319705186000` with `repetition_penalty` at 1.1. Only
bounding the schema does that.

## Context

**Roughly 128k tokens on a 36 GB Mac, roughly 64k on 32 GB.** The model's own
window is 262,144, but memory runs out first, and hum measures the real ceiling
for your machine at startup:

```sh
hum status          # Context   154,954 tokens, measured against this Mac's memory
curl -s localhost:4242/v1/models   # "context_length": 154954
```

Go past it and you get a `400` with `code: "context_length_exceeded"` rather
than a Mac that starts swapping.

Context is unusually cheap on this model — only 10 of its 40 layers keep a
growing KV cache, so 128k of history is about 5 GB. What actually limits you is
prefill: each chunk of the prompt attends against everything before it, and that
intermediate is transient but large. hum sizes the chunk from the prompt length
to hold it near 2 GB, which is what makes 128k fit at all.

Prefill is not free at that size: 11 s for 8k tokens, 385 s for 128k. You pay it
once per conversation rather than once per turn, because the prompt cache keeps
the prefix — which is also why an agent configured to compact at 128k will pay
it again on every compaction. Four conversations stay cached by default
(`--cache-entries`), each holding its own KV.

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

**Concurrency.** Requests are batched, so several callers share a decode step
rather than queueing behind each other. A step reads the whole model whatever
the batch size, which is why the aggregate goes up while each individual stream
slows down:

Measured with a different prompt from the table above, so compare rows
within it rather than across:

| clients | aggregate | per client |
|---|---|---|
| 1 | 85.5 tok/s | 85.5 |
| 2 | 95.7 tok/s | 47.9 |
| 4 | 173.9 tok/s | 43.5 |
| 8 | 213.6 tok/s | 26.7 |

Single-stream speed is unaffected — 89.7 tok/s measured after the change,
against 89-92 before it. Streaming and blocking callers mix freely, and the
context ceiling is shared between them rather than assumed by each.

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

Detokenisation is exact rather than approximate: the worker exports an
`id -> utf-8 bytes` table at startup and Go emits only complete runes.

### Prompt caching

The KV cache is snapshotted at the *stable* boundary of the conversation — the
history without the generation prompt — because the template appends
`<|im_start|>assistant\n<think>` at the end, and next turn the assistant's real
reply sits at that offset instead. Snapshot the whole prompt and the prefix
never matches again.

Snapshots are kept in an LRU (4 conversations by default), so switching between
chats does not throw the cache away.

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

## Limitations

- `stop` sequences are matched in the answer only, not inside the reasoning.
- Byte-level BPE detokenisation is verified on Qwen; the SPM path is written
  but untested.
- `/v1/chat/completions` and `/v1/models` only.
- Not implemented: `tool_choice`, `logprobs`, `n`, `seed`, LoRA.
- No auth and no rate limiting — see [Access](#access).
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

