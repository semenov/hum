package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func defs() []ToolDef {
	var d []ToolDef
	json.Unmarshal([]byte(`[
	 {"function":{"name":"get_weather","parameters":{"properties":{
	    "city":{"type":"string"},"days":{"type":"integer"},"metric":{"type":"boolean"}}}}},
	 {"function":{"name":"noop","parameters":{"properties":{}}}}
	]`), &d)
	return d
}

// collect feeds text in chunks of size n (n<=0 means all at once).
func collect(t *testing.T, text string, n int) (content, reasoning string, calls []ToolCall) {
	t.Helper()
	s := NewSplitter(defs())
	var evs []Event
	if n <= 0 {
		evs = append(evs, s.Push(text)...)
	} else {
		for i := 0; i < len(text); i += n {
			j := i + n
			if j > len(text) {
				j = len(text)
			}
			evs = append(evs, s.Push(text[i:j])...)
		}
	}
	evs = append(evs, s.Flush()...)
	for _, e := range evs {
		switch e.Kind {
		case evContent:
			content += e.Text
		case evReasoning:
			reasoning += e.Text
		case evToolCall:
			calls = append(calls, *e.Call)
		}
	}
	return
}

const sample = "<think>\nI should check.\n</think>\n\nSure.<tool_call>\n<function=get_weather>\n" +
	"<parameter=city>\nMoscow\n</parameter>\n<parameter=days>\n3\n</parameter>\n" +
	"<parameter=metric>\ntrue\n</parameter>\n</function>\n</tool_call>"

func TestParsesToolCall(t *testing.T) {
	c, r, calls := collect(t, sample, 0)
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	if calls[0].Name != "get_weather" {
		t.Errorf("name = %q", calls[0].Name)
	}
	// string stays a string; integer and boolean must NOT be quoted
	want := `{"city":"Moscow","days":3,"metric":true}`
	if calls[0].Args != want {
		t.Errorf("args = %s, want %s", calls[0].Args, want)
	}
	if !strings.Contains(r, "I should check") {
		t.Errorf("reasoning lost: %q", r)
	}
	if strings.Contains(c, "I should check") || strings.Contains(c, "<tool_call>") {
		t.Errorf("content polluted: %q", c)
	}
	if strings.TrimSpace(c) != "Sure." {
		t.Errorf("content = %q, want %q", c, "Sure.")
	}
}

// The real stream arrives one token at a time; tags land split across chunks.
func TestChunkingIsInvariant(t *testing.T) {
	c0, r0, k0 := collect(t, sample, 0)
	for _, n := range []int{1, 2, 3, 5, 7, 13} {
		c, r, k := collect(t, sample, n)
		if c != c0 || r != r0 || len(k) != len(k0) {
			t.Fatalf("chunk=%d mismatch:\n content %q vs %q\n reason %q vs %q\n calls %d vs %d",
				n, c, c0, r, r0, len(k), len(k0))
		}
		if len(k) > 0 && k[0].Args != k0[0].Args {
			t.Fatalf("chunk=%d args %s vs %s", n, k[0].Args, k0[0].Args)
		}
	}
}

func TestTwoCalls(t *testing.T) {
	txt := "<tool_call>\n<function=noop>\n</function>\n</tool_call>" +
		"<tool_call>\n<function=get_weather>\n<parameter=city>\nOslo\n</parameter>\n</function>\n</tool_call>"
	_, _, calls := collect(t, txt, 1)
	if len(calls) != 2 {
		t.Fatalf("want 2 calls, got %d", len(calls))
	}
	if calls[0].Args != "{}" || calls[1].Args != `{"city":"Oslo"}` {
		t.Errorf("args: %s | %s", calls[0].Args, calls[1].Args)
	}
}

func TestPlainTextUntouched(t *testing.T) {
	c, r, k := collect(t, "Just a normal answer with a < sign and <notatag>.", 1)
	if len(k) != 0 || r != "" {
		t.Fatalf("unexpected calls/reasoning: %d %q", len(k), r)
	}
	if c != "Just a normal answer with a < sign and <notatag>." {
		t.Errorf("content = %q", c)
	}
}

func TestResponseFormatGrammar(t *testing.T) {
	strict := true
	cases := []struct {
		name string
		rf   *ResponseFormat
		want string // JSON of what the worker should be told, "null" for nothing
	}{
		{"absent", nil, "null"},
		{"text is not a constraint", &ResponseFormat{Type: "text"}, "null"},
		{"json_object means an object", &ResponseFormat{Type: "json_object"},
			`{"type":"object"}`},
		{"json_schema passes the schema through", &ResponseFormat{
			Type: "json_schema",
			JSONSchema: &struct {
				Name   string          `json:"name"`
				Schema json.RawMessage `json:"schema"`
				Strict *bool           `json:"strict"`
			}{Name: "city", Schema: json.RawMessage(`{"type":"object","required":["a"]}`), Strict: &strict},
		}, `{"type":"object","required":["a"]}`},
		// A schema-less json_schema request is malformed, but answering it with
		// "any JSON" beats generating unconstrained text the caller will parse.
		{"json_schema without a schema", &ResponseFormat{Type: "json_schema"}, "true"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := json.Marshal(c.rf.grammar())
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("grammar() = %s, want %s", got, c.want)
			}
		})
	}
}

func TestStructuredOutputTurnsThinkingOff(t *testing.T) {
	no := false
	jsonReq := &ResponseFormat{Type: "json_object"}
	cases := []struct {
		name      string
		req       ChatReq
		wantThink bool
	}{
		{"plain request still thinks", ChatReq{}, true},
		{"schema alone turns it off", ChatReq{ResponseFormat: jsonReq}, false},
		// Asking for reasoning explicitly beats hum's preference, in all three
		// spellings the ecosystem uses.
		{"explicit effort wins", ChatReq{ResponseFormat: jsonReq, ReasoningEffort: "high"}, true},
		{"explicit object wins", ChatReq{ResponseFormat: jsonReq,
			Reasoning: &ReasoningReq{Effort: "low"}}, true},
		{"include_reasoning counts as asking", ChatReq{ResponseFormat: jsonReq,
			IncludeReason: &no}, true},
		{"text format is not structured", ChatReq{ResponseFormat: &ResponseFormat{Type: "text"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := c.req.reasoning()
			if c.req.ResponseFormat.grammar() != nil && !c.req.reasoningRequested() {
				plan.think = false
			}
			if plan.think != c.wantThink {
				t.Errorf("think = %v, want %v", plan.think, c.wantThink)
			}
		})
	}
}
