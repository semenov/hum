// A fast OpenAI-compatible server for MLX.
//
// Go owns everything per-token that is not the model itself: detokenization,
// stop-sequence matching, JSON/SSE framing. The Python worker does only
// prefill + the decode loop, so nothing contends with the generation thread.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

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
