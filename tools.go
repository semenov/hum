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
// events, and ends the stream at the first stop sequence. It withholds a short
// tail so a tag or stop split across two chunks is still recognised.
type Splitter struct {
	tools   map[string]ToolDef
	stops   []string // caller's stop sequences; content only, never returned
	hold    int      // the most that may need to be kept back
	buf     strings.Builder
	think   bool
	inTool  bool
	stopped bool
	toolBuf strings.Builder
}

func NewSplitter(defs []ToolDef, stops []string) *Splitter {
	m := make(map[string]ToolDef, len(defs))
	for _, d := range defs {
		if d.Function.Name != "" {
			m[d.Function.Name] = d
		}
	}
	s := &Splitter{tools: m}
	for _, st := range stops {
		if st != "" {
			s.stops = append(s.stops, st)
		}
	}
	// The longest marker minus one: a marker can be missing at most that much.
	for _, t := range allTags {
		s.hold = max(s.hold, len(t)-1)
	}
	for _, t := range s.stops {
		s.hold = max(s.hold, len(t)-1)
	}
	return s
}

// Stopped reports whether a stop sequence has matched. Nothing after it is
// emitted, and the caller should stop feeding the splitter.
func (s *Splitter) Stopped() bool { return s.stopped }

// markers are the strings whose start may be hiding at the end of a chunk.
// Stops only count outside the think block, where they could be emitted.
func (s *Splitter) markers() []string {
	if s.think {
		return allTags
	}
	return append(append([]string{}, allTags...), s.stops...)
}

// safeCut returns how much of text can be emitted without risking a split
// marker.
func (s *Splitter) safeCut(text string) int {
	limit := min(len(text), s.hold)
	markers := s.markers()
	for k := limit; k > 0; k-- {
		tail := text[len(text)-k:]
		for _, t := range markers {
			if strings.HasPrefix(t, tail) {
				return len(text) - k
			}
		}
	}
	return len(text)
}

func (s *Splitter) Push(chunk string) []Event {
	if s.stopped {
		return nil
	}
	s.buf.WriteString(chunk)
	return s.drain(false)
}

func (s *Splitter) Flush() []Event {
	if s.stopped {
		return nil
	}
	return s.drain(true)
}

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

		// find the earliest marker of interest
		idx, tag := -1, ""
		for _, t := range []string{tagToolOpen, tagThinkOpen, tagThinkClose} {
			if i := strings.Index(work, t); i >= 0 && (idx < 0 || i < idx) {
				idx, tag = i, t
			}
		}
		if !s.think {
			for _, t := range s.stops {
				if i := strings.Index(work, t); i >= 0 && (idx < 0 || i < idx) {
					idx, tag = i, t
				}
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
		default:
			// A stop sequence: the text before it has been emitted, the
			// sequence itself and everything after it are discarded.
			s.stopped = true
			s.buf.Reset()
			return out
		}
	}

	if s.inTool {
		return out // everything pending is already held in toolBuf
	}
	if final {
		out = append(out, s.emit(work)...)
		return out
	}
	cut := s.safeCut(work)
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
