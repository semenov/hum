// A fast OpenAI-compatible server for MLX.
//
// Go owns everything per-token that is not the model itself: detokenization,
// stop-sequence matching, JSON/SSE framing. The Python worker does only
// prefill + the decode loop, so nothing contends with the generation thread.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Passed straight through to the chat template so a tool round-trip can be
	// re-rendered on the next turn.
	ToolCalls  []any  `json:"tool_calls,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
}

type ChatReq struct {
	Model     string   `json:"model"`
	Messages  []Msg    `json:"messages"`
	MaxTokens int      `json:"max_tokens"`
	Temp      *float64 `json:"temperature"`
	TopP      *float64 `json:"top_p"`
	Stream    bool     `json:"stream"`
	Stop      []string `json:"stop"`
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
		"messages": r.Messages, "max_tokens": r.MaxTokens,
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

	writeMu sync.Mutex // one whole request line at a time
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
	// while other requests are still generating.
	ch := make(chan event, 1024)
	w.routes[id] = ch
	w.mu.Unlock()

	payload["id"] = id
	b, err := json.Marshal(payload)
	if err != nil {
		w.release(id)
		return 0, nil, err
	}
	w.writeMu.Lock()
	_, err = w.in.Write(append(b, '\n'))
	w.writeMu.Unlock()
	if err != nil {
		w.release(id)
		return 0, nil, err
	}
	return id, ch, nil
}

func (w *Worker) release(id uint32) {
	w.mu.Lock()
	delete(w.routes, id)
	w.mu.Unlock()
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
				if ch != nil {
					ch <- ev
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
		maxContext: maxContext, routes: map[uint32]chan event{}}
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

type event struct {
	kind byte // 'P' prefill done, 'T' token, 'E' end
	val  uint32
}

// recv waits for the next event of a request. A closed channel means the
// worker died.
func recv(ch chan event) (event, error) {
	ev, ok := <-ch
	if !ok {
		return event{}, fmt.Errorf("the worker stopped before finishing this request")
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

func (s *Server) chat(rw http.ResponseWriter, r *http.Request) {
	var req ChatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(rw, err.Error(), 400)
		return
	}
	if req.MaxTokens == 0 {
		// A thinking model can spend 512 tokens reasoning and return an empty
		// message, which is what a caller who sent no max_tokens sees first.
		// OpenAI's own default is the context window; this is a compromise that
		// leaves room to think and still bounds a runaway.
		req.MaxTokens = 4096
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
		http.Error(rw, err.Error(), 500)
		return
	}
	defer s.w.release(reqID)

	// The worker answers with the prompt length before it does anything with
	// it, so an oversized prompt costs a tokenisation rather than a swap storm.
	// 'C' (cache reuse) otherwise, which nothing downstream reads.
	first, err := recv(ch)
	if err != nil {
		http.Error(rw, err.Error(), 500)
		return
	}
	if first.kind == 'X' {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(rw).Encode(map[string]any{"error": map[string]any{
			"message": fmt.Sprintf("This prompt is %d tokens and the limit is %d, "+
				"which is what fits in this Mac's memory alongside the model's "+
				"weights. Send less, or run hum on a Mac with more memory.",
				first.val, s.w.maxContext),
			"type":  "invalid_request_error",
			"param": "messages",
			"code":  "context_length_exceeded",
		}})
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
	sp := NewSplitter(toolDefs)
	created := time.Now().Unix()

	var promptTok, genTok int

	// ---- non-streaming ----
	if !req.Stream {
		var content, reasoning strings.Builder
		var calls []ToolCall
		truncated := false
		for {
			ev, err := recv(ch)
			if err != nil {
				http.Error(rw, err.Error(), 500)
				return
			}
			if ev.kind == 'E' {
				genTok = int(ev.val)
				break
			}
			if ev.kind == 'P' {
				promptTok = int(ev.val)
				continue
			}
			if ev.kind == 'K' {
				sp.SetThinking(ev.val == 1)
				continue
			}
			if ev.kind == 'F' {
				truncated = ev.val == 1
				continue
			}
			if ev.kind != 'T' {
				continue
			}
			for _, e := range sp.Push(det.Add(ev.val)) {
				switch e.Kind {
				case evContent:
					content.WriteString(e.Text)
				case evReasoning:
					reasoning.WriteString(e.Text)
				case evToolCall:
					calls = append(calls, *e.Call)
				}
			}
		}
		for _, e := range sp.Flush() {
			switch e.Kind {
			case evContent:
				content.WriteString(e.Text)
			case evReasoning:
				reasoning.WriteString(e.Text)
			case evToolCall:
				calls = append(calls, *e.Call)
			}
		}
		msg := map[string]any{"role": "assistant", "content": content.String()}
		if reasoning.Len() > 0 && !plan.exclude {
			// Two spellings: OpenRouter reads "reasoning", LM Studio and others
			// read "reasoning_content".
			msg["reasoning_content"] = reasoning.String()
			msg["reasoning"] = reasoning.String()
		}
		finish := "stop"
		if truncated {
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
				"prompt_tokens": promptTok, "completion_tokens": genTok,
				"total_tokens": promptTok + genTok},
		})
		return
	}

	// ---- streaming ----
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.WriteHeader(200)
	fl, _ := rw.(http.Flusher)
	buf := bufio.NewWriterSize(rw, 1<<15)

	// Only the delta text changes per token, so the surrounding frame is built once.
	frame := func(field string) string {
		return fmt.Sprintf(`data: {"id":"%s","object":"chat.completion.chunk","created":%d,"model":%s,"choices":[{"index":0,"finish_reason":null,"delta":{"%s":`,
			id, created, mustJSON(s.model), field)
	}
	headContent, headReason, tail := frame("content"), frame("reasoning_content"), "}}]}\n\n"

	maxStop := 1
	for _, st := range req.Stop {
		if len(st) > maxStop {
			maxStop = len(st)
		}
	}
	var stopTail []byte
	var full strings.Builder
	nCalls := 0
	truncated := false
	stopped := false

	push := func(evs []Event) {
		for _, e := range evs {
			switch e.Kind {
			case evContent, evReasoning:
				if e.Kind == evReasoning && plan.exclude {
					continue // asked for, but not to be shown
				}
				h := headContent
				if e.Kind == evReasoning {
					h = headReason
				}
				buf.WriteString(h)
				writeJSONString(buf, e.Text)
				buf.WriteString(tail)
				if e.Kind == evContent && len(req.Stop) > 0 {
					full.WriteString(e.Text)
					stopTail = append(stopTail, e.Text...)
					if len(stopTail) > maxStop*2 {
						stopTail = append(stopTail[:0], stopTail[len(stopTail)-maxStop*2:]...)
					}
					for _, st := range req.Stop {
						if st != "" && strings.Contains(string(stopTail), st) {
							stopped = true
						}
					}
				}
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
		}
		buf.Flush()
		if fl != nil {
			fl.Flush()
		}
	}

	for !stopped {
		ev, err := recv(ch)
		if err != nil {
			return
		}
		if ev.kind == 'E' {
			genTok = int(ev.val)
			break
		}
		if ev.kind == 'P' {
			promptTok = int(ev.val)
			continue
		}
		if ev.kind == 'K' {
			sp.SetThinking(ev.val == 1)
			continue
		}
		if ev.kind == 'F' {
			truncated = ev.val == 1
			continue
		}
		if ev.kind != 'T' {
			continue
		}
		push(sp.Push(det.Add(ev.val)))
	}
	push(sp.Flush())

	finish := "stop"
	if truncated {
		finish = "length"
	}
	if nCalls > 0 {
		finish = "tool_calls"
	}
	fmt.Fprintf(buf, `data: {"id":"%s","object":"chat.completion.chunk","created":%d,"model":%s,"choices":[{"index":0,"finish_reason":"%s","delta":{}}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`+"\n\n",
		id, created, mustJSON(s.model), finish, promptTok, genTok, promptTok+genTok)
	buf.WriteString("data: [DONE]\n\n")
	buf.Flush()
	if fl != nil {
		fl.Flush()
	}
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
	w, err := NewWorker(cfg.Python, cfg.Worker, cfg.Model, cfg.CacheEntries)
	if err != nil {
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
	// what `hum status` reports.
	started := time.Now()
	mux.HandleFunc("/health", func(rw http.ResponseWriter, r *http.Request) {
		json.NewEncoder(rw).Encode(map[string]any{
			"status": "ok", "model": cfg.Model, "addr": cfg.Addr,
			"pid": os.Getpid(), "uptime_s": int(time.Since(started).Seconds()),
			"max_context": w.maxContext,
		})
	})
	var h http.Handler = mux
	if cfg.CORS {
		h = withCORS(mux)
	}
	log.Printf("listening on %s", cfg.Addr)
	return http.ListenAndServe(cfg.Addr, h)
}
