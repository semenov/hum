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
import json, os, queue, struct, sys, threading
from collections import deque
import mlx.core as mx
import llguidance
import llguidance.hf
import llguidance.mlx
import llguidance.numpy
from mlx_lm.utils import load
from mlx_lm.generate import BatchGenerator
from mlx_lm.models.cache import make_prompt_cache
from mlx_lm.sample_utils import make_sampler

MODEL_PATH, VOCAB_OUT = sys.argv[1], sys.argv[2]
CACHE_ENTRIES = int(sys.argv[3]) if len(sys.argv) > 3 else 4
out = sys.stdout.buffer
# How many prompt tokens to push through the model at once. The transient cost
# of one such step is roughly the chunk size times the context it attends
# against, so a chunk that is comfortable at 8k is what puts the process into
# swap at 128k. Measured on an M3 Max: 2048 tokens against 64k of context peaks
# at 26.8 GB, 512 against the same context at 22.6 GB, and the wired limit on a
# 36 GB machine is about 27. Rather than pick one constant and be wrong at one
# end, size the chunk so the transient stays near a budget: short prompts keep
# the fast path, long ones give up throughput in exchange for fitting at all.
PREFILL_BUDGET = 2 << 30      # bytes of transient we are willing to spend
PREFILL_PER_PAIR = 44         # measured bytes per (chunk token * context token)
PREFILL_CHUNK_MAX = 2048
PREFILL_CHUNK_MIN = 256
WATCH_DEBUG = os.environ.get("HUM_WATCH_DEBUG") == "1"
FIXED_CHUNK = int(os.environ.get("HUM_PREFILL_CHUNK", "0"))


def prefill_chunk(n_context):
    if FIXED_CHUNK:
        return FIXED_CHUNK
    if n_context <= 0:
        return PREFILL_CHUNK_MAX
    fits = PREFILL_BUDGET // (PREFILL_PER_PAIR * n_context)
    return max(PREFILL_CHUNK_MIN, min(PREFILL_CHUNK_MAX, int(fits)))

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


class _Watcher:
    """Base for logits processors that react to what the model has generated.

    generate_step hands a processor every token including the prompt, so the
    first call has to be treated as a starting line rather than as history:
    a prompt carrying an earlier `</think>` or `<tool_call>` from a previous
    turn would otherwise arm the grammar before generation has begun.
    """

    def __init__(self):
        self.seen = -1

    def _generated(self, tokens):
        if self.seen < 0:
            self.seen = tokens.size
            if WATCH_DEBUG:
                print(f"[watch] {type(self).__name__} first call sees "
                      f"{tokens.size} tokens: {tokens[-6:].tolist()}",
                      file=sys.stderr, flush=True)
            return []
        new = tokens[self.seen:].tolist()
        self.seen = tokens.size
        return new


class ToolGuard(_Watcher):
    """Logits processor: unconstrained until <tool_call>, grammar-locked after."""

    def __init__(self, tool_names):
        super().__init__()
        self.grammar = _grammar(tool_names)
        self.llt = _llg_tokenizer()
        self.matcher = None
        self.bitmask = None
        self.constrained = 0

    def __call__(self, tokens, logits):
        for t in self._generated(tokens):
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


class JSONGuard(_Watcher):
    """Logits processor that forces the answer to be JSON, optionally to schema.

    Armed only once the think block closes, so `response_format` and reasoning
    compose instead of excluding each other: the model reasons in prose and is
    then held to the shape. Once the value is complete the only legal token is
    the end of the message, otherwise the model cheerfully starts a second one.
    """

    def __init__(self, schema, wait_for_think):
        super().__init__()
        self.grammar = llguidance.LLMatcher.grammar_from_json_schema(
            True if schema is None else schema)
        self.llt = _llg_tokenizer()
        self.armed = not wait_for_think
        self.matcher = llguidance.LLMatcher(self.llt, self.grammar) if self.armed else None
        self.bitmask = None
        self.n_waited = 0
        self.waited_tail = []

    def _arm(self):
        self.armed = True
        self.matcher = llguidance.LLMatcher(self.llt, self.grammar)
        if WATCH_DEBUG:
            print(f"[watch] JSONGuard armed after {self.n_waited} tokens",
                  file=sys.stderr, flush=True)

    def __call__(self, tokens, logits):
        for t in self._generated(tokens):
            if not self.armed:
                self.n_waited += 1
                if WATCH_DEBUG:
                    self.waited_tail = (self.waited_tail + [t])[-12:]
                if t == THINK_CLOSE:
                    self._arm()
                continue
            if not self.matcher.is_stopped():
                self.matcher.consume_token(int(t))
        if not self.armed:
            return logits
        if self.matcher.get_error() or self.matcher.is_stopped():
            return _only(logits, next(iter(sorted(eos))))
        if self.bitmask is None:
            self.bitmask = llguidance.numpy.allocate_token_bitmask(1, self.llt.vocab_size)
        llguidance.numpy.fill_next_token_bitmask(self.matcher, self.bitmask, 0)
        return llguidance.mlx.apply_token_bitmask(logits, self.bitmask)


def _only(logits, token):
    """Mask everything but one token."""
    forced = mx.full(logits.shape, -mx.inf, dtype=logits.dtype)
    idx = mx.array([[token]])
    return mx.put_along_axis(forced, idx, logits[:, token:token + 1], axis=-1)


class ThinkBudget(_Watcher):
    """Force the think block shut once it has run long enough.

    There is no effort dial on the model, so a budget is the only honest way to
    express `reasoning.max_tokens` or a lower effort level: when the allowance
    is gone, mask every token except </think> and the model has to close it.
    """

    def __init__(self, budget, start_inside=False):
        super().__init__()
        self.budget = budget
        # The template puts <think> in the prompt rather than having the model
        # emit it, so the block is usually already open when generation starts.
        self.inside = start_inside
        self.spent = 0
        self.closed = False

    def __call__(self, tokens, logits):
        for t in self._generated(tokens):
            if t == THINK_OPEN:
                self.inside, self.spent = True, 0
            elif t == THINK_CLOSE:
                self.inside = False
            elif self.inside:
                self.spent += 1
        if not self.inside or self.spent < self.budget:
            return logits
        return _only(logits, THINK_CLOSE)   # allow only the closing token


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


def render(msgs, gen_prompt, tools=None, think=True):
    kw = {"tools": tools} if tools else {}
    if not think:
        # The template answers this by emitting an already-closed think block,
        # so the model starts on the answer instead of reasoning first.
        kw["enable_thinking"] = False
    p = tok.apply_chat_template(msgs, add_generation_prompt=gen_prompt, **kw)
    if isinstance(p, str):
        p = tok.encode(p, add_special_tokens=False)
    return list(p)


def stable_len(msgs, full, tools=None, think=True):
    """Length of the prefix of `full` that will still be a prefix on the next turn.

    The generation-prompt suffix (`<|im_start|>assistant\n<think>`) is NOT stable:
    next turn the template renders the assistant's actual reply at that offset
    instead. Rendering without add_generation_prompt gives exactly the history
    part, which the next turn can only append to.
    """
    st = render(msgs, False, tools, think)
    return len(st) if full[:len(st)] == st else 0


def prepare(ids):
    """Return (cache, tokens_to_prefill, n_reused)."""
    snap, n = history.fetch(ids)
    if snap is not None:
        return restore(snap), ids[n:], n
    return make_prompt_cache(model), ids, 0


def _nbytes(a):
    if isinstance(a, mx.array):
        return a.nbytes
    if isinstance(a, (list, tuple)):
        return sum(_nbytes(x) for x in a)
    return 0


def _cache_bytes(n):
    """Bytes of cache after running n tokens through the model.

    Measured on the cache arrays themselves, not on process memory: a forward
    pass also materialises logits, and at 248k vocab those are a gigabyte for a
    2048-token chunk, which swamps the thing being measured.
    """
    c = make_prompt_cache(model)
    model(mx.zeros((1, n), dtype=mx.uint32), cache=c)
    mx.eval([x.state for x in c])
    used = sum(_nbytes(x.state) for x in c)
    del c
    mx.clear_cache()
    return used


def kv_bytes_per_token(small=256, large=2048):
    """Measure what one token of context costs, rather than assume it.

    Two thirds of this model's layers are linear-attention whose state is a
    fixed size however long the conversation runs, and that fixed part is tens
    of megabytes. Measuring one prompt and dividing would charge all of it to
    the tokens in that prompt and undercount the ceiling by an order of
    magnitude. Two sizes subtract the constant and leave the slope.
    """
    lo, hi = _cache_bytes(small), _cache_bytes(large)
    per = (hi - lo) // (large - small)
    print(f"cache probe: {small} tok = {lo/2**20:.0f} MB, "
          f"{large} tok = {hi/2**20:.0f} MB, slope {per/1024:.1f} KB/token",
          file=sys.stderr, flush=True)
    return max(1, per)


def model_window():
    """The model's own limit, from whichever field the config uses."""
    try:
        cfg = json.load(open(os.path.join(MODEL_PATH, "config.json")))
    except Exception:
        return 0
    for d in (cfg, cfg.get("text_config") or {}):
        n = d.get("max_position_embeddings")
        if isinstance(n, int) and n > 0:
            return n
    return 0


def max_context():
    """Largest prompt this machine can hold, in tokens.

    Refusing an oversized prompt is the only way a client can find out; the
    OpenAI API has no field for it and the failure mode without it is not an
    error but a Mac that starts swapping. Metal reports the working set it is
    willing to give us, the model reports its own window, and the rest is
    measured: what is left after the weights, the prefill transient and some
    slack, divided by the cost of a token.
    """
    window = model_window() or 262144
    ws = mx.device_info().get("max_recommended_working_set_size", 0)
    if not ws:
        return window
    loaded = mx.get_active_memory()
    room = ws - loaded - PREFILL_BUDGET - (2 << 30)
    print(f"working set {ws/2**30:.1f} GB, weights {loaded/2**30:.1f} GB, "
          f"room {room/2**30:.1f} GB", file=sys.stderr, flush=True)
    if room <= 0:
        return 4096
    # The cache arrays are only half of what the process pays for them. A
    # KVCache grows by allocating a larger buffer and copying, so at the moment
    # of growth both exist, and measurement bears it out: the arrays come to
    # 20 KB per token on this model while resident memory grows by 40.
    per_token = 2 * kv_bytes_per_token()
    return max(4096, min(window, int(room // per_token)))


MAX_CONTEXT = max_context()
print(f"context ceiling {MAX_CONTEXT} tokens", file=sys.stderr, flush=True)
out.write(b"R" + struct.pack("<I", MAX_CONTEXT)); out.flush()

# ---- serve -----------------------------------------------------------------
# Requests are handled concurrently through mlx-lm's continuous batching. The
# win is not parallelism in the usual sense: a decode step reads the whole model
# whatever the batch size, so four sequences advancing together cost barely more
# than one advancing alone. Serialising them, as this worker used to, threw that
# away and made a second caller wait for the first to finish.
MAX_BATCH = int(os.environ.get("HUM_MAX_BATCH", "8"))

incoming = queue.Queue()


def _read_stdin():
    for line in sys.stdin:
        line = line.strip()
        if line:
            incoming.put(json.loads(line))
    incoming.put(None)


def emit(kind, rid, val):
    """Every event carries the request it belongs to; Go demultiplexes."""
    out.write(kind + struct.pack("<II", rid, val))


def chunked(seq, n):
    return [seq[i:i + n] for i in range(0, len(seq), n)]


class Live:
    """One in-flight request, from admission to its last token."""

    __slots__ = ("rid", "prompt", "cut", "snap_after", "segments_done",
                 "n_gen", "snapped", "budget")

    def __init__(self, rid, prompt, cut, snap_after, budget):
        self.rid = rid
        self.prompt = prompt
        self.cut = cut
        self.snap_after = snap_after   # segments to finish before snapshotting
        self.segments_done = 0
        self.n_gen = 0
        self.snapped = snap_after < 0
        self.budget = budget


class Admitted:
    """A request rendered and costed, waiting for room in the batch."""

    __slots__ = ("rid", "prompt", "cut", "segments", "snap_after", "cache",
                 "max_tokens", "sampler", "procs", "budget", "thinking",
                 "n_reused")

    def __init__(self, **kw):
        for k, v in kw.items():
            setattr(self, k, v)


def render_request(req):
    """Turn a request into something admittable, or refuse it outright.

    Returns None when the prompt cannot ever fit, having already told the
    client so — that is a property of the prompt, not of how busy we are.
    """
    rid = req["id"]
    tools = req.get("tools")
    think = req.get("enable_thinking", True)
    msgs = normalise(req["messages"])
    prompt = render(msgs, True, tools, think)

    if len(prompt) > MAX_CONTEXT:
        emit(b"X", rid, len(prompt))
        out.flush()
        print(f"refused {len(prompt)} tokens, ceiling {MAX_CONTEXT}",
              file=sys.stderr, flush=True)
        return None

    n_stable = stable_len(msgs, prompt, tools, think)
    cache, _, n_reused = prepare(prompt)

    cut = max(n_stable, n_reused)
    if cut <= n_reused or cut >= len(prompt):
        cut = len(prompt) - 1
    chunk = prefill_chunk(len(prompt))
    head_segs = chunked(prompt[n_reused:cut], chunk)
    tail_segs = chunked(prompt[cut:], chunk)
    segments = head_segs + tail_segs
    if not segments:
        segments = [prompt[-1:]]
    # Snapshot once the stable prefix is in the cache and before the generation
    # prompt goes in: that boundary is what the next turn can still match.
    snap_after = len(head_segs) if (cut == n_stable and head_segs) else -1

    procs = []
    schema = req.get("json_schema")
    if schema is not None:
        procs.append(JSONGuard(None if schema is True else schema,
                               think and reasoning_is_open(prompt)))
    elif os.environ.get("HUM_NO_GUARD") != "1":
        names = tool_names_of(tools)
        if names and TOOL_OPEN and TOOL_CLOSE:
            procs.append(ToolGuard(names))
    budget_think = int(req.get("think_budget") or 0)
    if think and budget_think > 0 and THINK_OPEN and THINK_CLOSE:
        procs.append(ThinkBudget(budget_think, reasoning_is_open(prompt)))

    max_tokens = int(req.get("max_tokens", 256))
    print(f"prompt {len(prompt)} | stable {n_stable} | reused {n_reused} "
          f"| prefill {len(prompt) - n_reused} | chunk {chunk} "
          f"| entries {len(history.entries)}", file=sys.stderr, flush=True)
    return Admitted(
        rid=rid, prompt=prompt, cut=cut, segments=segments, n_reused=n_reused,
        snap_after=snap_after, cache=cache, max_tokens=max_tokens,
        sampler=make_sampler(temp=req.get("temp", 0.7),
                             top_p=req.get("top_p", 1.0)),
        procs=procs or None,
        # What this request will hold at its widest: everything it has to keep
        # resident is prompt plus whatever it generates.
        budget=len(prompt) + max_tokens,
        thinking=reasoning_is_open(prompt),
    )


threading.Thread(target=_read_stdin, daemon=True).start()

gen = BatchGenerator(
    model,
    stop_tokens=[[t] for t in sorted(eos)],
    completion_batch_size=MAX_BATCH,
    prefill_batch_size=MAX_BATCH,
    prefill_step_size=PREFILL_CHUNK_MAX,
)

live = {}                 # uid -> Live
pending = deque()         # rendered, waiting for room
live_tokens = 0
closed = False

while True:
    # Drain stdin. Block only when there is genuinely nothing else to do,
    # otherwise a queued request would wait a whole decode step to be seen.
    while True:
        try:
            req = incoming.get(block=not (live or pending))
        except queue.Empty:
            break
        if req is None:
            closed = True
            break
        a = render_request(req)
        if a is not None:
            pending.append(a)
        if incoming.empty():
            break

    if closed and not live and not pending:
        break

    # Admit what fits. The context ceiling is a budget for the machine, not for
    # one request, so concurrent callers share it rather than each assuming it.
    while pending and len(live) < MAX_BATCH:
        a = pending[0]
        if live and live_tokens + a.budget > MAX_CONTEXT:
            break
        pending.popleft()
        uid = gen.insert_segments(
            [a.segments], max_tokens=[a.max_tokens], caches=[a.cache],
            samplers=[a.sampler],
            logits_processors=[a.procs] if a.procs else None,
        )[0]
        live[uid] = Live(a.rid, a.prompt, a.cut, a.snap_after, a.budget)
        live_tokens += a.budget
        emit(b"C", a.rid, a.n_reused)
        emit(b"K", a.rid, 1 if a.thinking else 0)
        out.flush()

    if not live:
        continue

    prompt_responses, generation_responses = gen.next()

    for r in prompt_responses:
        st = live.get(r.uid)
        if st is None:
            continue
        if r.end_of_segment:
            st.segments_done += 1
            if not st.snapped and st.segments_done == st.snap_after:
                st.snapped = True
                extracted = gen.extract_cache([r.uid]).get(r.uid)
                if extracted is not None:
                    history.insert(st.prompt[:st.cut],
                                   [c.state for c in extracted[0]])
        if r.end_of_prompt:
            emit(b"P", st.rid, len(st.prompt))
            out.flush()

    for r in generation_responses:
        st = live.get(r.uid)
        if st is None:
            continue
        # The stop token is a signal, not output; Go would detokenise it.
        if r.finish_reason != "stop":
            emit(b"T", st.rid, r.token)
            st.n_gen += 1
        if r.finish_reason is not None:
            emit(b"F", st.rid, 1 if r.finish_reason == "length" else 0)
            emit(b"E", st.rid, st.n_gen)
            live_tokens -= st.budget
            del live[r.uid]
        out.flush()
