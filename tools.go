package main

import (
	"encoding/json"
	"strings"
)

// Qwen3.5/3.6 native tool-call syntax, taken from the model's own
// chat_template.jinja:
//
//	<tool_call>
//	<function=NAME>
//	<parameter=KEY>
//	VALUE
//	</parameter>
//	</function>
//	</tool_call>
//
// The template renders string arguments verbatim and everything else through
// `tojson`, so decoding needs the parameter's declared type to invert it.
const (
	tagThinkOpen  = "<think>"
	tagThinkClose = "</think>"
	tagToolOpen   = "<tool_call>"
	tagToolClose  = "</tool_call>"
)

var allTags = []string{tagThinkOpen, tagThinkClose, tagToolOpen, tagToolClose}

// ToolDef is the subset of an OpenAI tool definition we need.
type ToolDef struct {
	Function struct {
		Name       string `json:"name"`
		Parameters struct {
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
		} `json:"parameters"`
	} `json:"function"`
}

type ToolCall struct {
	Name string
	Args string // JSON object, as OpenAI wants it: a *string*
}

type evKind int

const (
	evContent evKind = iota
	evReasoning
	evToolCall
)

type Event struct {
	Kind evKind
	Text string
	Call *ToolCall
}

// Splitter turns a raw token-text stream into content / reasoning / tool-call
// events. It withholds a short tail so a tag split across two chunks is still
// recognised.
type Splitter struct {
	tools   map[string]ToolDef
	buf     strings.Builder
	think   bool
	inTool  bool
	toolBuf strings.Builder
}

func NewSplitter(defs []ToolDef) *Splitter {
	m := make(map[string]ToolDef, len(defs))
	for _, d := range defs {
		if d.Function.Name != "" {
			m[d.Function.Name] = d
		}
	}
	return &Splitter{tools: m}
}

// maxHold is the longest tag minus one: the most we may need to keep back.
func maxHold() int {
	n := 0
	for _, t := range allTags {
		if len(t) > n {
			n = len(t)
		}
	}
	return n - 1
}

// safeCut returns how much of s can be emitted without risking a split tag.
func safeCut(s string) int {
	limit := len(s)
	if limit > maxHold() {
		limit = maxHold()
	}
	for k := limit; k > 0; k-- {
		tail := s[len(s)-k:]
		for _, t := range allTags {
			if strings.HasPrefix(t, tail) {
				return len(s) - k
			}
		}
	}
	return len(s)
}

func (s *Splitter) Push(chunk string) []Event {
	s.buf.WriteString(chunk)
	return s.drain(false)
}

func (s *Splitter) Flush() []Event { return s.drain(true) }

func (s *Splitter) drain(final bool) []Event {
	var out []Event
	work := s.buf.String()
	s.buf.Reset()

	for {
		if s.inTool {
			// The closing tag can itself be split across chunks, so search the
			// accumulated buffer rather than just the newly arrived text.
			s.toolBuf.WriteString(work)
			work = ""
			all := s.toolBuf.String()
			i := strings.Index(all, tagToolClose)
			if i < 0 {
				break
			}
			if c := s.parseCall(all[:i]); c != nil {
				out = append(out, Event{Kind: evToolCall, Call: c})
			}
			s.toolBuf.Reset()
			s.inTool = false
			work = all[i+len(tagToolClose):]
			continue
		}

		// find the earliest tag of interest
		idx, tag := -1, ""
		for _, t := range []string{tagToolOpen, tagThinkOpen, tagThinkClose} {
			if i := strings.Index(work, t); i >= 0 && (idx < 0 || i < idx) {
				idx, tag = i, t
			}
		}
		if idx < 0 {
			break
		}
		if idx > 0 {
			out = append(out, s.emit(work[:idx])...)
		}
		work = work[idx+len(tag):]
		switch tag {
		case tagToolOpen:
			s.inTool = true
		case tagThinkOpen:
			s.think = true
		case tagThinkClose:
			s.think = false
		}
	}

	if s.inTool {
		return out // everything pending is already held in toolBuf
	}
	if final {
		out = append(out, s.emit(work)...)
		return out
	}
	cut := safeCut(work)
	out = append(out, s.emit(work[:cut])...)
	s.buf.WriteString(work[cut:])
	return out
}

func (s *Splitter) emit(text string) []Event {
	if text == "" {
		return nil
	}
	if s.think {
		return []Event{{Kind: evReasoning, Text: text}}
	}
	return []Event{{Kind: evContent, Text: text}}
}

// parseCall turns "<function=NAME>\n<parameter=K>\nV\n</parameter>\n</function>"
// into a ToolCall with JSON-encoded arguments.
func (s *Splitter) parseCall(body string) *ToolCall {
	i := strings.Index(body, "<function=")
	if i < 0 {
		return nil
	}
	rest := body[i+len("<function="):]
	j := strings.Index(rest, ">")
	if j < 0 {
		return nil
	}
	name := strings.TrimSpace(rest[:j])
	rest = rest[j+1:]

	def, known := s.tools[name]
	args := map[string]any{}
	for {
		p := strings.Index(rest, "<parameter=")
		if p < 0 {
			break
		}
		rest = rest[p+len("<parameter="):]
		q := strings.Index(rest, ">")
		if q < 0 {
			break
		}
		key := strings.TrimSpace(rest[:q])
		rest = rest[q+1:]
		e := strings.Index(rest, "</parameter>")
		if e < 0 {
			break
		}
		raw := strings.Trim(rest[:e], "\n")
		rest = rest[e+len("</parameter>"):]

		typ := ""
		if known {
			if p, ok := def.Function.Parameters.Properties[key]; ok {
				typ = p.Type
			}
		}
		if typ == "string" {
			args[key] = raw
		} else {
			var v any
			if err := json.Unmarshal([]byte(raw), &v); err == nil {
				args[key] = v
			} else {
				args[key] = raw // model emitted something untyped; keep the text
			}
		}
	}
	b, err := json.Marshal(args)
	if err != nil {
		return nil
	}
	return &ToolCall{Name: name, Args: string(b)}
}

// SetThinking seeds the reasoning state. The chat template appends `<think>` to
// the generation prompt, so the model emits only the closing tag; without this
// the chain of thought would be misreported as content.
func (s *Splitter) SetThinking(open bool) { s.think = open }
