// Request and response shapes for the OpenAI-compatible API, and the
// normalisation that turns what a client sent into what the worker is told.
// Kept apart from the server so the translation can be tested on its own.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Content is a message body. OpenAI accepts either a string or a list of
// parts, and the SDKs' multimodal helpers, LangChain and the Vercel AI SDK all
// send the list form even for plain text. Only text parts are meaningful
// here; anything else is refused with a reason rather than a decode error.
type Content string

func (c *Content) UnmarshalJSON(b []byte) error {
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) == 0 || string(b) == "null" {
		*c = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*c = Content(s)
		return nil
	}
	if b[0] == '[' {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(b, &parts); err != nil {
			return err
		}
		var texts []string
		for _, p := range parts {
			switch p.Type {
			case "text", "":
				texts = append(texts, p.Text)
			default:
				return fmt.Errorf("content part of type %q is not supported; hum takes text only", p.Type)
			}
		}
		*c = Content(strings.Join(texts, "\n"))
		return nil
	}
	return fmt.Errorf("content must be a string or a list of text parts")
}

type Msg struct {
	Role    string  `json:"role"`
	Content Content `json:"content"`
	// Passed straight through to the chat template so a tool round-trip can be
	// re-rendered on the next turn.
	ToolCalls  []any  `json:"tool_calls,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
}

type ChatReq struct {
	Model    string `json:"model"`
	Messages []Msg  `json:"messages"`
	// max_completion_tokens is the current OpenAI spelling; max_tokens the one
	// every client still sends. Either is accepted, the new one wins.
	MaxTokens           int      `json:"max_tokens"`
	MaxCompletionTokens int      `json:"max_completion_tokens"`
	Temp                *float64 `json:"temperature"`
	TopP                *float64 `json:"top_p"`
	Stream              bool     `json:"stream"`
	Stop                []string `json:"stop"`
	// Kept raw so the chat template receives the full schema (descriptions,
	// required, nested types). A typed view is decoded separately for parsing.
	Tools []json.RawMessage `json:"tools"`

	ResponseFormat *ResponseFormat `json:"response_format"`

	// Sampling penalties. frequency and presence are OpenAI's; repetition is
	// not, but every local runtime has it and it is the one that actually
	// breaks a token the model has got stuck on.
	FrequencyPenalty  *float64           `json:"frequency_penalty"`
	PresencePenalty   *float64           `json:"presence_penalty"`
	RepetitionPenalty *float64           `json:"repetition_penalty"`
	LogitBias         map[string]float64 `json:"logit_bias"`

	// Reasoning control, in all three shapes the ecosystem uses.
	Reasoning       *ReasoningReq `json:"reasoning"`
	ReasoningEffort string        `json:"reasoning_effort"`
	IncludeReason   *bool         `json:"include_reasoning"`
}

// defaultMaxTokens bounds a request that named no limit. A thinking model can
// spend 512 tokens reasoning and return an empty message, which is what a
// caller who sent nothing sees first. OpenAI's own default is the context
// window; this is a compromise that leaves room to think and still bounds a
// runaway.
const defaultMaxTokens = 4096

// maxTokens resolves the two spellings and the default.
func (r ChatReq) maxTokens() int {
	if r.MaxCompletionTokens > 0 {
		return r.MaxCompletionTokens
	}
	if r.MaxTokens > 0 {
		return r.MaxTokens
	}
	return defaultMaxTokens
}

// ResponseFormat is OpenAI's structured-output request. "text" is the default
// and means nothing is constrained; "json_object" asks only for valid JSON;
// "json_schema" carries a schema the answer has to satisfy.
type ResponseFormat struct {
	Type       string `json:"type"`
	JSONSchema *struct {
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
		Strict *bool           `json:"strict"`
	} `json:"json_schema"`
}

// grammar returns what the worker should constrain generation to: a JSON
// schema, `true` for any JSON, or nil to leave generation alone.
func (r *ResponseFormat) grammar() any {
	if r == nil {
		return nil
	}
	switch r.Type {
	case "json_object":
		// Any JSON value is technically valid JSON, and a model asked for one
		// with no other guidance will happily answer `1.5`. Everyone who sets
		// this means an object.
		return map[string]any{"type": "object"}
	case "json_schema":
		if r.JSONSchema != nil && len(r.JSONSchema.Schema) > 0 {
			return r.JSONSchema.Schema
		}
		return true
	}
	return nil
}

// ReasoningReq is OpenRouter's object. effort and max_tokens both bound how
// long the model may think; exclude keeps the thinking out of the reply but
// still lets it happen.
type ReasoningReq struct {
	Effort    string `json:"effort"`
	MaxTokens int    `json:"max_tokens"`
	Exclude   bool   `json:"exclude"`
	Enabled   *bool  `json:"enabled"`
}

// reasoningPlan is the normalised form the worker is told about.
type reasoningPlan struct {
	think   bool // let the model reason at all
	budget  int  // tokens it may spend on it, 0 for no limit
	exclude bool // reason, but do not return it
}

// effortBudget maps the named levels onto token budgets. There is no dial on
// the model itself, so effort is expressed as how long it may think.
func effortBudget(effort string) (think bool, budget int, ok bool) {
	switch strings.ToLower(effort) {
	case "none":
		return false, 0, true
	case "minimal":
		return true, 256, true
	case "low":
		return true, 1024, true
	case "medium":
		return true, 4096, true
	case "high":
		return true, 0, true
	}
	return true, 0, false
}

// reasoningRequested reports whether the caller said anything about reasoning
// at all, in any of the three spellings. Silence is what lets hum choose.
func (r ChatReq) reasoningRequested() bool {
	return r.Reasoning != nil || r.ReasoningEffort != "" || r.IncludeReason != nil
}

func (r ChatReq) reasoning() reasoningPlan {
	p := reasoningPlan{think: true}
	if r.ReasoningEffort != "" {
		if th, b, ok := effortBudget(r.ReasoningEffort); ok {
			p.think, p.budget = th, b
		}
	}
	// include_reasoning is the legacy spelling: false means exclude.
	if r.IncludeReason != nil && !*r.IncludeReason {
		p.exclude = true
	}
	if r.Reasoning != nil {
		q := r.Reasoning
		if q.Effort != "" {
			if th, b, ok := effortBudget(q.Effort); ok {
				p.think, p.budget = th, b
			}
		}
		if q.MaxTokens > 0 {
			p.budget = q.MaxTokens
		}
		if q.Enabled != nil && !*q.Enabled {
			p.think = false
		}
		if q.Exclude {
			p.exclude = true
		}
	}
	return p
}

// workerPayload is everything the worker needs about a request. Kept apart
// from the handler so what crosses the pipe can be tested without a model
// behind it.
func (r ChatReq) workerPayload(plan reasoningPlan, temp, topP float64) map[string]any {
	return map[string]any{
		"messages": r.Messages, "max_tokens": r.maxTokens(),
		"temp": temp, "top_p": topP, "tools": r.Tools,
		"enable_thinking": plan.think, "think_budget": plan.budget,
		"json_schema":       r.ResponseFormat.grammar(),
		"frequency_penalty": r.FrequencyPenalty, "presence_penalty": r.PresencePenalty,
		"repetition_penalty": r.RepetitionPenalty, "logit_bias": r.LogitBias,
	}
}
