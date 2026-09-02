"""Deterministic check that the tool grammar actually rejects invalid calls.
Tests the grammar+tokenizer integration without depending on model behaviour."""
import json, os, sys
import llguidance, llguidance.hf
from transformers import AutoTokenizer

# The model hum downloads on first start; pass another path as the argument.
M = sys.argv[1] if len(sys.argv) > 1 else os.path.expanduser(
    "~/.hum/models/lmstudio-community__Qwen3.6-35B-A3B-MLX-4bit")
tok = AutoTokenizer.from_pretrained(M)
V = max(int(tok.vocab_size), max(int(i) for i in tok.get_vocab().values()) + 1)
llt = llguidance.hf.from_tokenizer(tok, n_vocab=V, eos_token=[248046, 248044])
CLOSE = tok.encode("</tool_call>", add_special_tokens=False)[0]

def grammar(names):
    choice = " | ".join(json.dumps(n) for n in names)
    return rf"""%llguidance {{}}
start: WS "<function=" tool ">" WS parameter* "</function>" WS <[{CLOSE}]>
tool: {choice}
parameter: "<parameter=" PARAM_NAME ">" param_value WS
PARAM_NAME: /[^>]/+
param_value[suffix="</parameter>"]: SAFE_PARAM_VALUE
SAFE_PARAM_VALUE: /(?s:.*)/ & ~/(?s:.*)<\/(function|tool_call)>(?s:.*)/
WS: /[ \t\n\r]*/
"""

G = grammar(("get_weather", "get_time"))

def feed(text, append_close=True):
    """Feed text token by token; return (accepted, tokens_consumed)."""
    m = llguidance.LLMatcher(llt, G)
    ids = tok.encode(text, add_special_tokens=False)
    if append_close:
        ids = ids + [CLOSE]
    for k, t in enumerate(ids):
        m.consume_token(int(t))
        if m.get_error():
            return False, k
    return not m.get_error(), len(ids)

VALID = "\n<function=get_weather>\n<parameter=city>\nMoscow\n</parameter>\n</function>\n"
cases = [
    ("valid call",                           VALID,                                              True),
    ("second allowed tool",                  "\n<function=get_time>\n</function>\n",             True),
    ("made-up function name",                "\n<function=get_stock_price>\n</function>\n",      False),
    ("prefix of an allowed name",            "\n<function=get_weath>\n</function>\n",            False),
    ("wrong format (JSON instead of tags)",  '\n{"name":"get_weather"}\n',                       False),
    ("missing closing </function>",          "\n<function=get_weather>\n",                      False),
]
fails = 0
for name, text, want_ok in cases:
    ok, n = feed(text)
    mark = "OK " if ok == want_ok else "FAIL"
    if ok != want_ok:
        fails += 1
    print(f"  [{mark}] {name:36} accepted={ok!s:5} expected={want_ok}")
print("\nRESULT:", "all checks passed" if fails == 0 else f"{fails} failed")
sys.exit(1 if fails else 0)
