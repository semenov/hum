// A fast OpenAI-compatible server for MLX.
//
// Go owns everything per-token that is not the model itself: detokenization,
// stop-sequence matching, JSON/SSE framing. The Python worker does only
// prefill + the decode loop, so nothing contends with the generation thread.
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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

// ---- worker ---------------------------------------------------------------

type Worker struct {
	cmd   *exec.Cmd
	in    io.WriteCloser
	out   *bufio.Reader
	vocab [][]byte
	// Largest prompt this machine can hold, measured by the worker against the
	// weights it actually loaded. Published so clients can plan, enforced so
	// they do not have to.
	maxContext int

	// Requests run concurrently, so events arrive interleaved and tagged with
	// the request they belong to. One goroutine reads them and posts each to
	// the handler waiting for it.
	mu     sync.Mutex
	nextID uint32
	routes map[uint32]chan event
	dead   error
	died   chan struct{} // closed once, when the worker stops

	writeMu sync.Mutex // one whole line at a time
}

// submit registers a request and hands it to the worker, returning the channel
// its events will arrive on. Callers must release the id when they are done,
// finished or not, or the routing table grows forever.
func (w *Worker) submit(payload map[string]any) (uint32, chan event, error) {
	w.mu.Lock()
	if w.dead != nil {
		err := w.dead
		w.mu.Unlock()
		return 0, nil, err
	}
	id := w.nextID
	w.nextID++
	// Buffered generously: the pump must not stall behind one slow HTTP client
	// while other requests are still generating. If the buffer does fill, the
	// pump treats the client as gone rather than waiting for it (see pump).
	ch := make(chan event, 1024)
	w.routes[id] = ch
	w.mu.Unlock()

	payload["id"] = id
	b, err := json.Marshal(payload)
	if err != nil {
		w.release(id)
		return 0, nil, err
	}
	if err := w.writeLine(b); err != nil {
		w.release(id)
		return 0, nil, err
	}
	return id, ch, nil
}

func (w *Worker) writeLine(b []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	_, err := w.in.Write(append(b, '\n'))
	return err
}

func (w *Worker) release(id uint32) {
	w.mu.Lock()
	delete(w.routes, id)
	w.mu.Unlock()
}

// cancel tells the worker to drop a request. A sequence that nobody is
// reading still shares every decode step with the others, so an abandoned one
// slows everyone down until its max_tokens run out.
func (w *Worker) cancel(id uint32) {
	b, _ := json.Marshal(map[string]any{"cancel": id})
	_ = w.writeLine(b)
}

func (w *Worker) deadErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dead
}

// pump reads the worker's event stream and routes each event to its request.
// It is the only reader of w.out.
func (w *Worker) pump() {
	for {
		k, err := w.out.ReadByte()
		if err == nil {
			var b [8]byte
			_, err = io.ReadFull(w.out, b[:])
			if err == nil {
				id := binary.LittleEndian.Uint32(b[:4])
				ev := event{k, binary.LittleEndian.Uint32(b[4:])}
				w.mu.Lock()
				ch := w.routes[id]
				w.mu.Unlock()
				if ch == nil {
					continue
				}
				select {
				case ch <- ev:
				default:
					// The handler has not drained a thousand events: its client
					// is not reading. Blocking here would freeze every other
					// request, so this one is dropped instead.
					w.release(id)
					close(ch)
					w.cancel(id)
				}
				continue
			}
		}
		// The worker died or the pipe closed. Everyone waiting has to be told,
		// or they wait forever.
		w.mu.Lock()
		w.dead = fmt.Errorf("worker stopped: %w", err)
		for _, ch := range w.routes {
			close(ch)
		}
		w.routes = map[uint32]chan event{}
		w.mu.Unlock()
		close(w.died)
		return
	}
}

func NewWorker(python, script, model string, entries int) (*Worker, error) {
	vf, err := os.CreateTemp("", "vocab-*.bin")
	if err != nil {
		return nil, err
	}
	vf.Close()
	defer os.Remove(vf.Name())

	cmd := exec.Command(python, script, model, vf.Name(), strconv.Itoa(entries))
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	br := bufio.NewReaderSize(stdout, 1<<16)
	log.Println("loading model in worker...")
	b, err := br.ReadByte() // wait for 'R'
	if err != nil || b != 'R' {
		return nil, fmt.Errorf("worker failed to start (got %q, err %v)", b, err)
	}
	var ctxBuf [4]byte
	if _, err := io.ReadFull(br, ctxBuf[:]); err != nil {
		return nil, fmt.Errorf("worker did not report a context ceiling: %w", err)
	}
	maxContext := int(binary.LittleEndian.Uint32(ctxBuf[:]))

	vocab, err := loadVocab(vf.Name())
	if err != nil {
		return nil, err
	}
	log.Printf("worker ready, vocab %d entries, context ceiling %d tokens",
		len(vocab), maxContext)
	w := &Worker{cmd: cmd, in: stdin, out: br, vocab: vocab,
		maxContext: maxContext, routes: map[uint32]chan event{},
		died: make(chan struct{})}
	go w.pump()
	return w, nil
}

func loadVocab(path string) ([][]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(raw[:4])
	v := make([][]byte, n)
	off := 4
	for i := uint32(0); i < n; i++ {
		l := int(binary.LittleEndian.Uint16(raw[off : off+2]))
		off += 2
		v[i] = raw[off : off+l]
		off += l
	}
	return v, nil
}

// event is one message from the worker. Kinds: 'X' prompt over the ceiling,
// '!' request could not be rendered, 'C' cache reuse, 'K' thinking state,
// 'P' prefill done, 'T' token, 'F' truncated flag, 'E' end.
type event struct {
	kind byte
	val  uint32
}

// errClosed means the request's channel was closed under the handler: either
// the worker died or the pump gave up on this client.
var errClosed = errors.New("request channel closed")

// recv waits for the next event of a request.
func recv(ch chan event) (event, error) {
	ev, ok := <-ch
	if !ok {
		return event{}, errClosed
	}
	return ev, nil
}

// ---- streaming detokenizer (byte-level BPE) --------------------------------

type Detok struct {
	vocab [][]byte
	buf   []byte // bytes not yet emitted (incomplete utf-8)
}

// Add returns the newly decodable text for this token.
func (d *Detok) Add(id uint32) string {
	if int(id) < len(d.vocab) {
		d.buf = append(d.buf, d.vocab[id]...)
	}
	// emit only complete utf-8 runes
	good := len(d.buf)
	for good > 0 {
		r, sz := utf8.DecodeLastRune(d.buf[:good])
		if r == utf8.RuneError && sz <= 1 {
			good-- // trailing partial rune, hold it back
			if len(d.buf)-good > 4 {
				good = len(d.buf) // not a partial rune, it's genuinely invalid
				break
			}
			continue
		}
		break
	}
	if good == 0 {
		return ""
	}
	s := string(d.buf[:good])
	d.buf = append(d.buf[:0], d.buf[good:]...)
	return s
}

// ---- server ---------------------------------------------------------------

// ModelID is what the API calls the model, whatever is actually loaded. A
// client's configuration should not have to change because the weights did,
// and a filesystem path is a poor thing to paste into an editor's settings.
const ModelID = "hum"

type Server struct {
	w     *Worker
	model string
}

// apiError answers in the shape the OpenAI SDKs parse, so a client shows the
// message rather than "unexpected response".
func apiError(rw http.ResponseWriter, status int, msg, typ, code string) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	json.NewEncoder(rw).Encode(map[string]any{"error": map[string]any{
		"message": msg, "type": typ, "code": code, "param": nil,
	}})
}

// outcome is what a request produced besides its text.
type outcome struct {
	promptTok, genTok int
	truncated         bool // max_tokens ran out
	stopped           bool // a stop sequence matched
}

// consume drives one request from its first token to its end, handing every
// content, reasoning and tool-call event to emit. Shared by the streaming and
// blocking paths so stop sequences and cancellation behave the same in both.
func (s *Server) consume(id uint32, ch chan event, det *Detok, sp *Splitter, emit func(Event)) (outcome, error) {
	var o outcome
	for {
		ev, err := recv(ch)
		if err != nil {
			return o, err
		}
		switch ev.kind {
		case 'E':
			for _, e := range sp.Flush() {
				emit(e)
			}
			return o, nil
		case 'P':
			o.promptTok = int(ev.val)
		case 'K':
			sp.SetThinking(ev.val == 1)
		case 'F':
			o.truncated = ev.val == 1
		case 'T':
			o.genTok++
			for _, e := range sp.Push(det.Add(ev.val)) {
				emit(e)
			}
			if sp.Stopped() {
				// The rest of the sequence is not wanted; do not let the worker
				// spend decode steps on it.
				o.stopped = true
				s.w.cancel(id)
				return o, nil
			}
		}
	}
}

func (s *Server) chat(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		rw.Header().Set("Allow", http.MethodPost)
		apiError(rw, http.StatusMethodNotAllowed, "use POST for chat completions",
			"invalid_request_error", "method_not_allowed")
		return
	}
	var req ChatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(rw, http.StatusBadRequest, "could not read the request: "+err.Error(),
			"invalid_request_error", "invalid_json")
		return
	}
	if len(req.Messages) == 0 {
		apiError(rw, http.StatusBadRequest, "messages must not be empty",
			"invalid_request_error", "invalid_request")
		return
	}
	temp := 0.7
	if req.Temp != nil {
		temp = *req.Temp
	}
	topP := 1.0
	if req.TopP != nil {
		topP = *req.TopP
	}

	plan := req.reasoning()
	// Structured output and long reasoning do not mix well on this model.
	// After a thousand tokens of prose the model tends to write a number the
	// way a person would, with separators, and the grammar forbids a comma
	// inside a JSON number -- so the only tokens left are more digits, and it
	// emits zeros until it runs out of budget. Measured: 8 of 8 schema
	// requests parse with reasoning off, 2 of 4 with it on. A caller who names
	// a reasoning setting still gets exactly what they asked for.
	if req.ResponseFormat.grammar() != nil && !req.reasoningRequested() {
		plan.think = false
	}
	reqID, ch, err := s.w.submit(req.workerPayload(plan, temp, topP))
	if err != nil {
		apiError(rw, http.StatusServiceUnavailable, err.Error(), "server_error", "worker_unavailable")
		return
	}
	done := make(chan struct{})
	defer close(done)
	defer s.w.release(reqID)
	// A client that hangs up mid-request should not keep the model busy.
	go func() {
		select {
		case <-r.Context().Done():
			s.w.cancel(reqID)
		case <-done:
		}
	}()

	// The worker answers with the prompt length before it does anything with
	// it, so an oversized prompt costs a tokenisation rather than a swap storm.
	// 'C' (cache reuse) otherwise, which nothing downstream reads.
	first, err := recv(ch)
	if err != nil {
		apiError(rw, http.StatusInternalServerError, s.closedReason(), "server_error", "worker_stopped")
		return
	}
	switch first.kind {
	case 'X':
		apiError(rw, http.StatusBadRequest,
			fmt.Sprintf("This prompt is %d tokens and the limit is %d, which is what "+
				"fits in this Mac's memory alongside the model's weights. Send less, "+
				"or run hum on a Mac with more memory.", first.val, s.w.maxContext),
			"invalid_request_error", "context_length_exceeded")
		return
	case '!':
		apiError(rw, http.StatusBadRequest,
			"The worker could not turn these messages into a prompt; `hum logs` has the traceback.",
			"invalid_request_error", "unrenderable_request")
		return
	}

	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	det := &Detok{vocab: s.w.vocab}
	toolDefs := make([]ToolDef, 0, len(req.Tools))
	for _, raw := range req.Tools {
		var d ToolDef
		if json.Unmarshal(raw, &d) == nil {
			toolDefs = append(toolDefs, d)
		}
	}
	sp := NewSplitter(toolDefs, req.Stop)
	created := time.Now().Unix()

	// ---- non-streaming ----
	if !req.Stream {
		var content, reasoning strings.Builder
		var calls []ToolCall
		o, err := s.consume(reqID, ch, det, sp, func(e Event) {
			switch e.Kind {
			case evContent:
				content.WriteString(e.Text)
			case evReasoning:
				reasoning.WriteString(e.Text)
			case evToolCall:
				calls = append(calls, *e.Call)
			}
		})
		if err != nil {
			apiError(rw, http.StatusInternalServerError, s.closedReason(), "server_error", "worker_stopped")
			return
		}
		msg := map[string]any{"role": "assistant", "content": content.String()}
		if reasoning.Len() > 0 && !plan.exclude {
			// Two spellings: OpenRouter reads "reasoning", LM Studio and others
			// read "reasoning_content".
			msg["reasoning_content"] = reasoning.String()
			msg["reasoning"] = reasoning.String()
		}
		finish := "stop"
		if o.truncated {
			finish = "length"
		}
		if len(calls) > 0 {
			finish = "tool_calls"
			msg["tool_calls"] = toolCallsJSON(calls, id)
		}
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(map[string]any{
			"id": id, "object": "chat.completion", "model": s.model,
			"created": created,
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": finish, "message": msg}},
			"usage": map[string]int{
				"prompt_tokens": o.promptTok, "completion_tokens": o.genTok,
				"total_tokens": o.promptTok + o.genTok},
		})
		return
	}

	// ---- streaming ----
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.WriteHeader(200)
	fl, _ := rw.(http.Flusher)
	buf := bufio.NewWriterSize(rw, 1<<15)
	flush := func() {
		buf.Flush()
		if fl != nil {
			fl.Flush()
		}
	}

	// Only the delta text changes per token, so the surrounding frame is built once.
	frame := func(field string) string {
		return fmt.Sprintf(`data: {"id":"%s","object":"chat.completion.chunk","created":%d,"model":%s,"choices":[{"index":0,"finish_reason":null,"delta":{"%s":`,
			id, created, mustJSON(s.model), field)
	}
	headContent, headReason, tail := frame("content"), frame("reasoning_content"), "}}]}\n\n"

	// OpenAI opens every stream by naming the role; strict clients wait for it.
	fmt.Fprintf(buf, `data: {"id":"%s","object":"chat.completion.chunk","created":%d,"model":%s,"choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":""}}]}`+"\n\n",
		id, created, mustJSON(s.model))
	flush()

	nCalls := 0
	o, err := s.consume(reqID, ch, det, sp, func(e Event) {
		switch e.Kind {
		case evContent, evReasoning:
			if e.Kind == evReasoning && plan.exclude {
				return // asked for, but not to be shown
			}
			h := headContent
			if e.Kind == evReasoning {
				h = headReason
			}
			buf.WriteString(h)
			writeJSONString(buf, e.Text)
			buf.WriteString(tail)
		case evToolCall:
			b, _ := json.Marshal(map[string]any{
				"index": nCalls, "id": fmt.Sprintf("%s-call-%d", id, nCalls),
				"type":     "function",
				"function": map[string]string{"name": e.Call.Name, "arguments": e.Call.Args},
			})
			fmt.Fprintf(buf, `data: {"id":"%s","object":"chat.completion.chunk","created":%d,"model":%s,"choices":[{"index":0,"finish_reason":null,"delta":{"tool_calls":[%s]}}]}`+"\n\n",
				id, created, mustJSON(s.model), b)
			nCalls++
		}
		flush()
	})
	if err != nil {
		// The stream is already open, so the only honest thing is to end it.
		fmt.Fprintf(buf, `data: {"error":{"message":%s,"type":"server_error","code":"worker_stopped"}}`+"\n\n",
			mustJSON(s.closedReason()))
		buf.WriteString("data: [DONE]\n\n")
		flush()
		return
	}

	finish := "stop"
	if o.truncated {
		finish = "length"
	}
	if nCalls > 0 {
		finish = "tool_calls"
	}
	fmt.Fprintf(buf, `data: {"id":"%s","object":"chat.completion.chunk","created":%d,"model":%s,"choices":[{"index":0,"finish_reason":"%s","delta":{}}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`+"\n\n",
		id, created, mustJSON(s.model), finish, o.promptTok, o.genTok, o.promptTok+o.genTok)
	buf.WriteString("data: [DONE]\n\n")
	flush()
}

// closedReason explains a channel closed under a handler.
func (s *Server) closedReason() string {
	if err := s.w.deadErr(); err != nil {
		return "the worker stopped before finishing this request: " + err.Error()
	}
	return "the request was dropped because the client stopped reading"
}

// withCORS lets a page in a browser talk to the server. Off unless asked for:
// with it on, any site the user visits can reach this port, since the request
// comes from their own machine and the network never sees it.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Access-Control-Allow-Origin", "*")
		rw.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		rw.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		rw.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			rw.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(rw, r)
	})
}

func toolCallsJSON(calls []ToolCall, id string) []any {
	out := make([]any, 0, len(calls))
	for i, c := range calls {
		out = append(out, map[string]any{
			"index": i, "id": fmt.Sprintf("%s-call-%d", id, i), "type": "function",
			"function": map[string]string{"name": c.Name, "arguments": c.Args},
		})
	}
	return out
}

func mustJSON(s string) string { b, _ := json.Marshal(s); return string(b) }

// writeJSONString writes a JSON-quoted string without allocating a map/struct.
func writeJSONString(w *bufio.Writer, s string) {
	w.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			w.WriteString(`\"`)
		case c == '\\':
			w.WriteString(`\\`)
		case c == '\n':
			w.WriteString(`\n`)
		case c == '\r':
			w.WriteString(`\r`)
		case c == '\t':
			w.WriteString(`\t`)
		case c < 0x20:
			fmt.Fprintf(w, `\u%04x`, c)
		default:
			w.WriteByte(c)
		}
	}
	w.WriteByte('"')
}

// runServer loads the model and serves until killed. This is the foreground
// path; `hum start` re-execs the binary into it as a detached child.
func runServer(cfg Config) error {
	// Bind before loading: a taken port should fail in a millisecond, not after
	// twenty seconds and twenty gigabytes.
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w", cfg.Addr, err)
	}
	w, err := NewWorker(cfg.Python, cfg.Worker, cfg.Model, cfg.CacheEntries)
	if err != nil {
		ln.Close()
		return err
	}
	s := &Server{w: w, model: ModelID}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.chat)
	mux.HandleFunc("/v1/models", func(rw http.ResponseWriter, r *http.Request) {
		json.NewEncoder(rw).Encode(map[string]any{"object": "list",
			"data": []any{map[string]any{
				"id": ModelID, "object": "model", "owned_by": "hum",
				// None of these are in the OpenAI schema, but this is the only
				// place a client can find out what it is talking to and how much
				// of it there is. context_length is the spelling OpenRouter and
				// OpenCode already read.
				"name": prettyModel(cfg.Model), "path": cfg.Model,
				"context_length": w.maxContext,
			}}})
	})
	// Health is what `hum start` polls to know the model finished loading, and
	// what `hum status` reports. It is only "ok" while the worker is alive.
	started := time.Now()
	mux.HandleFunc("/health", func(rw http.ResponseWriter, r *http.Request) {
		status := "ok"
		if err := w.deadErr(); err != nil {
			status = "worker stopped"
			rw.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(rw).Encode(map[string]any{
			"status": status, "model": cfg.Model, "addr": cfg.Addr,
			"pid": os.Getpid(), "uptime_s": int(time.Since(started).Seconds()),
			"max_context": w.maxContext,
		})
	})
	var h http.Handler = mux
	if cfg.CORS {
		h = withCORS(mux)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	// A server without its worker is a zombie: it answers /health, `hum status`
	// says running, and every request fails. Better to leave with the worker
	// so the pid file goes stale and `hum start` works again.
	go func() {
		<-w.died
		log.Printf("worker stopped; shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()
	log.Printf("listening on %s", cfg.Addr)
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		if derr := w.deadErr(); derr != nil {
			return derr
		}
		return nil
	}
	return err
}
