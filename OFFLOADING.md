# Handing work to hum from Claude Code or Codex

This is not about replacing the model in your coding agent. A 35B model with 3B
active parameters will not out-think a frontier one, and pointing Claude Code or
Codex at it wholesale makes both worse.

It is about the other thing hum is: a capable model that costs nothing per
token, has no rate limit, never leaves your Mac, and will take eight requests at
once. Your expensive model's scarce resources are money and context. hum's
scarce resource is judgement. So give hum the work that consumes context and
tokens without needing judgement, and keep the judgement where it belongs.

## The rule

Hand it over when all three hold:

1. **It is mechanical or bulk** — the same small task over many inputs, or one
   large input that needs squashing.
2. **You can check the answer** — you will read the summary, run the test, see
   the diff. Verification is cheap.
3. **It costs context rather than thought** — a 4,000-line log, forty files,
   a diff you only need the shape of.

Keep it yourself when being subtly wrong is expensive and hard to notice:
architecture, security review, anything touching auth or money, precise edits
across several files, and any decision you would want to justify later.

## First, is it up

```sh
hum status                      # or:
curl -s localhost:4242/health   # {"status":"ok", ...}
```

If it is not, `hum start`. Do not fall back to doing bulk work yourself
silently — say the local model was unavailable.

## Recipes

Every number below was measured on an M3 Max, not estimated.

### Compress before you read

The most valuable one. A build log, a test run, a long diff — squash it locally
and spend your own context on the answer instead of the input.

```sh
cat build.log | hum ask -quiet "Which tests failed and why? Two lines each."
git diff main... | hum ask -quiet "Summarise this diff as a bullet list of behaviour changes."
```

`hum ask` reads stdin, `-quiet` prints only the answer, so it drops straight
into a pipe.

### Fan out over files

hum serves eight requests concurrently, and a decode step reads the whole model
whatever the batch size — so eight at once cost barely more than one.

```sh
ls *.go | xargs -P 8 -I{} sh -c \
  'printf "%-16s %s\n" "{}" "$(head -60 {} | hum ask -quiet "One sentence: what is this file for?")"'
```

Measured on this repo, eight files: **42 s at `-P 8`, 96 s at `-P 1`**. Use
`-P 8`; beyond that they queue.

Good for: first-pass triage of an unfamiliar codebase, finding which of forty
files mention a concept, drafting docstrings, classifying records.

### Get structured data back

For anything you will parse, ask for a schema rather than prose. The grammar is
enforced during generation, so the shape is not a request, it is a guarantee.

```sh
curl -s localhost:4242/v1/chat/completions -H 'content-type: application/json' -d '{
  "model": "hum", "max_tokens": 600,
  "response_format": {"type": "json_schema", "json_schema": {"name": "triage", "schema": {
    "type": "object",
    "properties": {
      "severity":  {"type": "string", "enum": ["low", "medium", "high"]},
      "component": {"type": "string"},
      "one_line":  {"type": "string"}},
    "required": ["severity", "component", "one_line"],
    "additionalProperties": false}}},
  "messages": [{"role": "user", "content": "Triage this log:\n..."}]}'
```

Two things that matter:

- **Bound your numbers.** `{"type": "integer"}` with no `maximum` lets the model
  emit digits forever, and no grammar may stop it, because one more digit is
  always valid JSON. Give it a `maximum`.
- **Still validate what comes back.** Not because the grammar fails — it does
  not — but because a malformed request that silently loses `response_format`
  looks like a model that ignored you.

### Draft the boring text

```sh
git diff --cached | hum ask -quiet "Write a one-line commit message, imperative mood."
```

## What this is worth

Single stream is ~90 tok/s and eight concurrent streams total ~215 tok/s. A
fan-out of forty files is a couple of minutes and costs nothing. The same work
through a paid API is real money and real rate limits; through your own context
it is worse than that, because context is the thing you cannot buy more of
mid-task.

## Paste this into your agent's instructions

For Claude Code put it in `CLAUDE.md`, for Codex in `AGENTS.md`. Both read the
file at the repo root.

```markdown
## Local model for bulk work

A local LLM runs at http://127.0.0.1:4242/v1 (OpenAI-compatible, model id
`hum`). It is free, unlimited, and handles 8 requests at once. Check with
`hum status`; start with `hum start`.

Use it for work that is mechanical, bulk, or context-hungry, and that I can
verify: summarising long logs and diffs before you read them, first-pass
descriptions of many files, classification, drafting docstrings and commit
messages. Fan out with `xargs -P 8` — eight at once is roughly the cost of one.

    cat build.log | hum ask -quiet "which tests failed and why?"
    ls *.go | xargs -P 8 -I{} sh -c 'head -60 {} | hum ask -quiet "one line: what is this for?"'

For anything you will parse, use `response_format` with a JSON schema and give
numeric fields a `maximum`.

Do not use it for architecture decisions, security review, or precise edits
across several files — it is a 35B model and it will be confidently wrong in
ways that are expensive to catch. Do not use it silently: say what you handed
over, and check the result before relying on it.
```
