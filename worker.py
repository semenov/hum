"""Minimal MLX generation worker with prompt-cache reuse.

Does ONLY: chat template -> tokenize -> prefill -> decode loop -> emit token ids.
No detokenization, no stop-string matching, no JSON, no SSE. All of that is Go's job.

Protocol (binary, over stdout):
  startup : writes vocab byte-table to argv[2], then b'R' (ready)
  request : one JSON line on stdin {"messages":[...],"max_tokens":N,"temp":F}
  reply   : b'C' + u32 n_tokens_reused   (prompt-cache hit length)
            b'P' + u32 n_prompt_tokens   (prefill done -> TTFT marker)
            b'T' + u32 token_id          (repeated)
            b'E' + u32 n_generated       (done)
"""
import json, os, struct, sys
import mlx.core as mx
import llguidance
import llguidance.hf
import llguidance.mlx
import llguidance.numpy
from mlx_lm.utils import load
from mlx_lm.generate import generate_step
from mlx_lm.models.cache import make_prompt_cache
from mlx_lm.sample_utils import make_sampler

MODEL_PATH, VOCAB_OUT = sys.argv[1], sys.argv[2]
CACHE_ENTRIES = int(sys.argv[3]) if len(sys.argv) > 3 else 4
out = sys.stdout.buffer
PREFILL_CHUNK = 2048

model, tok = load(MODEL_PATH)
mx.eval(model.parameters())


# ---- export id -> raw utf-8 bytes so Go can detokenize ---------------------
def build_byte_table(tokenizer):
    d = tokenizer.detokenizer
    n = max(tokenizer.vocab.values()) + 1
    table = [b""] * n
    if hasattr(d, "_byte_decoder"):                 # BPE (byte-level, e.g. Qwen)
        bd = d._byte_decoder
        for s, i in tokenizer.vocab.items():
            ba = bytearray()
            for c in s:
                v = bd.get(c, None)
                if v is None: ba.extend(c.encode())
                else: ba.append(v)
            table[i] = bytes(ba)
    elif hasattr(d, "tokenmap"):                    # SPM
        for i, v in enumerate(d.tokenmap):
            if i < n: table[i] = v if isinstance(v, bytes) else str(v).encode()
    else:
        for s, i in tokenizer.vocab.items():
            table[i] = tokenizer.convert_tokens_to_string([s]).encode()
    return table


with open(VOCAB_OUT, "wb") as f:
    table = build_byte_table(tok)
    f.write(struct.pack("<I", len(table)))
    for b in table:
        f.write(struct.pack("<H", len(b))); f.write(b)
del table

eos = set(tok.eos_token_ids) if getattr(tok, "eos_token_ids", None) else {tok.eos_token_id}


def _id_of(piece):
    try:
        ids = tok.encode(piece, add_special_tokens=False)
        return ids[0] if len(ids) == 1 else None
    except Exception:
        return None


THINK_OPEN, THINK_CLOSE = _id_of("<think>"), _id_of("</think>")
TOOL_OPEN, TOOL_CLOSE = _id_of("<tool_call>"), _id_of("</tool_call>")

# ---- constrained decoding for tool calls ----------------------------------
# Parsing the model's output is best-effort: a malformed call is silently lost
# and nothing stops the model naming a function that was never offered. Instead
# we drive generation with a grammar and mask the logits, so an invalid tool
# call cannot be produced at all. Grammar is the one from the model's own
# syntax; masking is only armed once the model has emitted <tool_call>.
_VOCAB = max(int(tok._tokenizer.vocab_size),
             max(int(i) for i in tok._tokenizer.get_vocab().values()) + 1)
_LLG_TOKENIZER = None


def _llg_tokenizer():
    global _LLG_TOKENIZER
    if _LLG_TOKENIZER is None:
        _LLG_TOKENIZER = llguidance.hf.from_tokenizer(
            tok._tokenizer, n_vocab=_VOCAB, eos_token=sorted(int(t) for t in eos))
    return _LLG_TOKENIZER


def _grammar(tool_names):
    choice = " | ".join(json.dumps(n) for n in tool_names)
    return rf"""%llguidance {{}}
start: WS "<function=" tool ">" WS parameter* "</function>" WS <[{TOOL_CLOSE}]>
tool: {choice}
parameter: "<parameter=" PARAM_NAME ">" param_value WS
PARAM_NAME: /[^>]/+
param_value[suffix="</parameter>"]: SAFE_PARAM_VALUE
SAFE_PARAM_VALUE: /(?s:.*)/ & ~/(?s:.*)<\/(function|tool_call)>(?s:.*)/
WS: /[ \t\n\r]*/
"""


class ToolGuard:
    """Logits processor: unconstrained until <tool_call>, grammar-locked after."""

    def __init__(self, tool_names):
        self.grammar = _grammar(tool_names)
        self.llt = _llg_tokenizer()
        self.matcher = None
        self.seen = 0
        self.bitmask = None
        self.constrained = 0

    def __call__(self, tokens, logits):
        new = tokens[self.seen:].tolist()
        self.seen = tokens.size
        for t in new:
            if self.matcher is None:
                if t == TOOL_OPEN:
                    self.matcher = llguidance.LLMatcher(self.llt, self.grammar)
                    self.constrained = 0
            else:
                self.matcher.consume_token(int(t))
                self.constrained += 1
                if self.matcher.get_error() or self.matcher.is_stopped():
                    self.matcher = None
        if self.matcher is None:
            return logits
        if self.bitmask is None:
            self.bitmask = llguidance.numpy.allocate_token_bitmask(1, self.llt.vocab_size)
        llguidance.numpy.fill_next_token_bitmask(self.matcher, self.bitmask, 0)
        return llguidance.mlx.apply_token_bitmask(logits, self.bitmask)


def tool_names_of(tools):
    out = []
    for t in tools or []:
        fn = t.get("function") if isinstance(t, dict) else None
        if isinstance(fn, dict) and isinstance(fn.get("name"), str) and fn["name"]:
            out.append(fn["name"])
    return tuple(dict.fromkeys(out))


def reasoning_is_open(ids):
    """True if the prompt ends inside a <think> block.

    The template appends `<think>` to the generation prompt, so the model never
    emits an opening tag - only the closing one. Without this the whole chain of
    thought would be reported as content.
    """
    for t in reversed(ids):
        if t == THINK_OPEN:
            return True
        if t == THINK_CLOSE:
            return False
    return False


def normalise(msgs):
    """OpenAI sends tool_call.arguments as a JSON *string*; the chat template
    does `arguments|items` and needs a mapping."""
    out = []
    for m in msgs:
        tc = m.get("tool_calls")
        if tc:
            m = dict(m)
            fixed = []
            for c in tc:
                c = dict(c)
                fn = c.get("function")
                if isinstance(fn, dict) and isinstance(fn.get("arguments"), str):
                    fn = dict(fn)
                    try:
                        fn["arguments"] = json.loads(fn["arguments"])
                    except json.JSONDecodeError:
                        fn["arguments"] = {}
                    c["function"] = fn
                fixed.append(c)
            m["tool_calls"] = fixed
        out.append(m)
    return out


# ---- prompt cache reuse ----------------------------------------------------
# The snapshot is taken right after prefill and BEFORE generation, so it holds
# exactly the prompt. That matters: the generated tokens must not be in it,
# because the chat template re-renders the assistant turn differently (this is
# a thinking model) and the cache would no longer be a prefix of the next
# prompt. Rolling back is not an option here — 30 of 40 layers are linear
# attention (ArraysCache), whose recurrent state is not trimmable.
class PromptCacheLRU:
    """Keeps snapshots for several conversations, not just the most recent one.

    An entry is usable only if its token list is a full prefix of the incoming
    prompt, since the recurrent state of the linear-attention layers cannot be
    rolled back. Longest usable prefix wins.
    """

    def __init__(self, capacity):
        self.capacity = capacity
        self.entries = []          # least-recently-used first

    def fetch(self, ids):
        best, best_n = None, 0
        for e in self.entries:
            t = e[0]
            if len(t) <= best_n or len(t) > len(ids) - 1:
                continue
            if ids[:len(t)] == t:
                best, best_n = e, len(t)
        if best is None:
            return None, 0
        self.entries.remove(best); self.entries.append(best)   # touch
        return best[1], best_n

    def insert(self, tokens, snap):
        tokens = list(tokens)
        self.entries = [e for e in self.entries if e[0] != tokens]
        self.entries.append((tokens, snap))
        while len(self.entries) > self.capacity:
            self.entries.pop(0)


history = PromptCacheLRU(CACHE_ENTRIES)


def _copy(a):
    if isinstance(a, mx.array): return mx.array(a)
    if isinstance(a, (list, tuple)):
        v = [_copy(x) for x in a]
        return tuple(v) if isinstance(a, tuple) else v
    return a


def take_snapshot(cache):
    return [c.state for c in cache]


def restore(snap):
    """Build a fresh cache from a snapshot. Copies, so the snapshot is never
    handed to a live cache that would mutate it."""
    cache = make_prompt_cache(model)
    for c, st in zip(cache, snap):
        c.state = _copy(st)
    return cache


def render(msgs, gen_prompt, tools=None):
    kw = {"tools": tools} if tools else {}
    p = tok.apply_chat_template(msgs, add_generation_prompt=gen_prompt, **kw)
    if isinstance(p, str):
        p = tok.encode(p, add_special_tokens=False)
    return list(p)


def stable_len(msgs, full, tools=None):
    """Length of the prefix of `full` that will still be a prefix on the next turn.

    The generation-prompt suffix (`<|im_start|>assistant\n<think>`) is NOT stable:
    next turn the template renders the assistant's actual reply at that offset
    instead. Rendering without add_generation_prompt gives exactly the history
    part, which the next turn can only append to.
    """
    st = render(msgs, False, tools)
    return len(st) if full[:len(st)] == st else 0


def prepare(ids):
    """Return (cache, tokens_to_prefill, n_reused)."""
    snap, n = history.fetch(ids)
    if snap is not None:
        return restore(snap), ids[n:], n
    return make_prompt_cache(model), ids, 0


out.write(b"R"); out.flush()

# ---- serve -----------------------------------------------------------------
for line in sys.stdin:
    line = line.strip()
    if not line: continue
    req = json.loads(line)
    tools = req.get("tools")
    msgs = normalise(req["messages"])
    prompt = render(msgs, True, tools)
    n_stable = stable_len(msgs, prompt, tools)

    cache, todo, n_reused = prepare(prompt)
    out.write(b"C" + struct.pack("<I", n_reused))
    out.write(b"K" + struct.pack("<I", 1 if reasoning_is_open(prompt) else 0))
    print(f"prompt {len(prompt)} | stable {n_stable} | reused {n_reused} "
          f"| prefill {len(prompt) - n_reused} | entries {len(history.entries)}",
          file=sys.stderr, flush=True)

    # Prefill up to the stable boundary and snapshot there, then prefill the
    # rest (the generation-prompt suffix, which next turn will not match).
    cut = max(n_stable, n_reused)
    if cut <= n_reused or cut >= len(prompt):
        cut = len(prompt) - 1        # nothing stable to snapshot; keep >=1 token
    def prefill(seq):
        for i in range(0, len(seq), PREFILL_CHUNK):
            model(mx.array(seq[i:i + PREFILL_CHUNK])[None], cache=cache)
            mx.eval([c.state for c in cache])
            mx.clear_cache()   # mlx-lm does this between prefill chunks;
                               # without it allocation pressure builds up
    prefill(prompt[n_reused:cut])
    if cut == n_stable:
        history.insert(prompt[:cut], take_snapshot(cache))
    prefill(prompt[cut:-1])
    todo = prompt[-1:]

    sampler = make_sampler(temp=req.get("temp", 0.7), top_p=req.get("top_p", 1.0))
    names = tool_names_of(tools)
    guard_on = os.environ.get("HUM_NO_GUARD") != "1"
    procs = ([ToolGuard(names)]
             if (guard_on and names and TOOL_OPEN and TOOL_CLOSE) else None)
    n = 0
    first = True
    w = out.write
    for tid, _ in generate_step(mx.array(todo), model,
                                max_tokens=req.get("max_tokens", 256),
                                sampler=sampler, prompt_cache=cache,
                                logits_processors=procs):
        if first:
            w(b"P" + struct.pack("<I", len(prompt))); out.flush(); first = False
        if tid in eos: break
        w(b"T" + struct.pack("<I", tid)); out.flush()
        n += 1
    if first:
        w(b"P" + struct.pack("<I", len(prompt)))
    w(b"E" + struct.pack("<I", n)); out.flush()
