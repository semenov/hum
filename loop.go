package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// turn is one message in the conversation, in the shape the server expects.
type turn map[string]any

type toolCallOut struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type reply struct {
	Content   string
	Reasoning string
	Calls     []toolCallOut
	Finish    string
	Tokens    int
}

// ask sends one request and returns the whole reply. The agent loop needs the
// tool calls as a unit, so this does not stream.
func ask(addr string, msgs []turn, tools json.RawMessage, maxTok int) (reply, error) {
	body := map[string]any{
		"model": "hum", "messages": msgs, "stream": false, "max_tokens": maxTok,
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post("http://"+addr+"/v1/chat/completions",
		"application/json", bytes.NewReader(b))
	if err != nil {
		return reply{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		e, _ := io.ReadAll(resp.Body)
		return reply{}, fmt.Errorf("server returned %s: %s", resp.Status, strings.TrimSpace(string(e)))
	}
	var out struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   string        `json:"content"`
				Reasoning string        `json:"reasoning_content"`
				ToolCalls []toolCallOut `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return reply{}, err
	}
	if len(out.Choices) == 0 {
		return reply{}, fmt.Errorf("the server returned no choices")
	}
	c := out.Choices[0]
	return reply{c.Message.Content, c.Message.Reasoning, c.Message.ToolCalls,
		c.FinishReason, out.Usage.CompletionTokens}, nil
}

// agentLoop runs think-act-observe until the model stops asking for tools.
// It returns the final answer and the conversation, so a REPL can carry on.
func agentLoop(u *UI, addr string, msgs []turn, o agentOpts, verbose bool) ([]turn, string, error) {
	tools := toolsFor(o)
	for step := 0; step < maxSteps; step++ {
		r, err := ask(addr, msgs, tools, 16384)
		if err != nil {
			return msgs, "", err
		}
		m := turn{"role": "assistant", "content": r.Content}
		if len(r.Calls) > 0 {
			m["tool_calls"] = r.Calls
		}
		msgs = append(msgs, m)

		if len(r.Calls) == 0 {
			if r.Content == "" && r.Finish == "length" {
				return msgs, "", fmt.Errorf("the model used its whole token budget without answering")
			}
			return msgs, r.Content, nil
		}

		for _, call := range r.Calls {
			name := call.Function.Name
			if verbose {
				fmt.Printf("    %s %s %s\n", u.p.blue("→"), u.p.bold(name),
					u.p.dim(oneLine(call.Function.Arguments)))
			}
			result := runTool(u, o, name, call.Function.Arguments)
			if verbose {
				fmt.Printf("    %s %s\n", u.p.dim("←"), u.p.dim(oneLine(result)))
			}
			msgs = append(msgs, turn{
				"role": "tool", "tool_call_id": call.ID, "name": name, "content": result,
			})
		}
	}
	return msgs, "", fmt.Errorf("stopped after %d steps without finishing", maxSteps)
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
	if len(s) > 88 {
		s = s[:88] + "…"
	}
	return s
}

// ---- commands --------------------------------------------------------------

// askCmd answers a single question with no tools. Reads stdin when piped, so
// `cat file | hum ask "summarise this"` works.
func askCmd(cfg Config, query string) error {
	u := newUI()
	if err := requireServer(u, cfg); err != nil {
		return err
	}
	if piped := readPiped(); piped != "" {
		query = query + "\n\n" + piped
	}
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("nothing to ask — pass a question, or pipe text in")
	}
	r, err := ask(cfg.Addr, []turn{{"role": "user", "content": query}}, nil, 16384)
	if err != nil {
		return err
	}
	if r.Content == "" {
		return fmt.Errorf("the model returned no answer (finish_reason: %s)", r.Finish)
	}
	fmt.Println(strings.TrimSpace(r.Content))
	return nil
}

// runCmd is the agent, non-interactive: it works to a result and prints it.
func runCmd(cfg Config, query string, allowWrite, allowShell, quiet bool) error {
	u := newUI()
	if err := requireServer(u, cfg); err != nil {
		return err
	}
	if piped := readPiped(); piped != "" {
		query = query + "\n\n" + piped
	}
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("nothing to do — pass a task to run")
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	o := agentOpts{root: root, allowWrite: allowWrite, allowShell: allowShell, interactive: false}
	if !quiet {
		u.Head("RUNNING", oneLine(query))
		var granted []string
		if allowWrite {
			granted = append(granted, "write files inside this directory")
		}
		if allowShell {
			granted = append(granted, "run shell commands, sandboxed so they cannot write outside it")
		}
		if len(granted) == 0 {
			u.Para("It can read and search only. There is nobody here to confirm a " +
				"change, so --allow-write and --allow-shell have to be granted on the command line.")
		} else {
			u.Para("It may %s.", strings.Join(granted, ", and "))
		}
	}
	msgs := []turn{{"role": "system", "content": agentSystem}, {"role": "user", "content": query}}
	_, answer, err := agentLoop(u, cfg.Addr, msgs, o, !quiet)
	if err != nil {
		return err
	}
	if !quiet {
		fmt.Println()
	}
	fmt.Println(strings.TrimSpace(answer))
	return nil
}

func requireServer(u *UI, cfg Config) error {
	if _, alive := readPID(); !alive {
		u.Off("Hum is not running.")
		u.Para("This needs a server to talk to.")
		u.Hint("Start it with", "hum start")
		return errQuiet
	}
	if _, err := probe(cfg.Addr, 5*time.Second); err != nil {
		return fmt.Errorf("the server is not answering on %s — try `hum status`", cfg.Addr)
	}
	return nil
}

// errQuiet means the reason was already explained; the caller should just exit.
var errQuiet = fmt.Errorf("")

// readPiped returns stdin when it is not a terminal, so text can be piped in.
func readPiped() string {
	st, _ := os.Stdin.Stat()
	if st == nil || st.Mode()&os.ModeCharDevice != 0 {
		return ""
	}
	b, err := io.ReadAll(bufio.NewReader(os.Stdin))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
