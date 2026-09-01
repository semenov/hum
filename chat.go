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

// chatCmd is a small REPL against the local server, so the thing can be tried
// without wiring up a client first.
func chatCmd(cfg Config) error {
	u := newUI()

	// chat is a client, nothing more: it neither downloads nor starts anything.
	if _, alive := readPID(); !alive {
		u.Off("Hum is not running.")
		u.Para("Chat needs a server to talk to. Start one first — the model is " +
			"downloaded on the first run, which takes a while, and loaded on every " +
			"run, which takes a few seconds.")
		u.Hint("Start it with", "hum start")
		return nil
	}
	h, err := probe(cfg.Addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("the server is not answering on %s — try `hum status`", cfg.Addr)
	}

	u.OK("Chatting with %s", modelLabel(h.Model))
	u.Para("Type a message and press enter. This model thinks before it answers, " +
		"so there is a short pause before the reply appears. Use /think to watch " +
		"it reason, /reset for a fresh conversation, /exit to leave.")

	var history []map[string]string
	showThinking := false
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 1<<20), 1<<20)

	for {
		fmt.Printf("  %s ", u.p.blue("you ›"))
		if !in.Scan() {
			fmt.Println()
			break
		}
		text := strings.TrimSpace(in.Text())
		switch {
		case text == "":
			continue
		case text == "/exit" || text == "/quit":
			fmt.Println()
			return nil
		case text == "/reset":
			history = nil
			fmt.Printf("  %s\n\n", u.p.dim("(conversation cleared)"))
			continue
		case text == "/think":
			showThinking = !showThinking
			state := "hidden"
			if showThinking {
				state = "shown"
			}
			fmt.Printf("  %s\n\n", u.p.dim("(reasoning is now "+state+")"))
			continue
		case strings.HasPrefix(text, "/"):
			fmt.Printf("  %s\n\n", u.p.dim("commands: /think, /reset, /exit"))
			continue
		}

		history = append(history, map[string]string{"role": "user", "content": text})
		fmt.Printf("\n  %s ", u.p.green("hum ›"))

		reply, st, err := streamChat(cfg.Addr, history, u, showThinking)
		if err != nil {
			fmt.Println()
			u.Fail("%s", err)
			history = history[:len(history)-1] // do not keep a turn that failed
			continue
		}
		history = append(history, map[string]string{"role": "assistant", "content": reply})
		fmt.Printf("\n\n  %s\n\n", u.p.dim(st))
	}
	fmt.Println()
	return nil
}

// streamChat sends one turn and prints the reply as it arrives, returning the
// assistant text and a one-line summary of the timings.
func streamChat(addr string, history []map[string]string, u *UI, showThinking bool) (string, string, error) {
	body, _ := json.Marshal(map[string]any{
		"model": "hum", "messages": history, "stream": true, "max_tokens": 2048,
	})
	resp, err := http.Post("http://"+addr+"/v1/chat/completions",
		"application/json", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("server returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	var answer strings.Builder
	start := time.Now()
	var thoughtFor time.Duration
	var ttft time.Duration
	n := 0
	thinking := false
	lastTick := time.Time{}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(line[6:]), &ev) != nil || len(ev.Choices) == 0 {
			continue
		}
		d := ev.Choices[0].Delta
		// The chain of thought is long and mostly noise on a first look, so by
		// default it collapses to a ticking counter. Silence would read as a hang.
		if d.Reasoning != "" {
			n++
			if showThinking {
				if !thinking {
					fmt.Print(u.p.dim("(thinking) "))
					thinking = true
				}
				fmt.Print(u.p.dim(d.Reasoning))
			} else if u.p.on && time.Since(lastTick) > 200*time.Millisecond {
				lastTick = time.Now()
				thinking = true
				fmt.Printf("\r\033[K  %s ", u.p.dim(fmt.Sprintf("(thinking… %ds)",
					int(time.Since(start).Seconds()))))
			}
		}
		if d.Content != "" {
			if ttft == 0 {
				// Time to the first token of the answer, which is what is
				// actually waited on — the reasoning arrives immediately.
				ttft = time.Since(start)
				thoughtFor = ttft
				// The reply opens with the blank lines left by the think block.
				d.Content = strings.TrimLeft(d.Content, " \n")
				if d.Content == "" {
					continue
				}
			}
			if thinking {
				if showThinking {
					fmt.Printf("\n\n  %s ", u.p.green("hum ›"))
				} else {
					fmt.Printf("\r\033[K  %s ", u.p.green("hum ›"))
				}
				thinking = false
			}
			fmt.Print(d.Content)
			answer.WriteString(d.Content)
			n++
		}
	}
	total := time.Since(start)
	rate := 0.0
	if total > 0 && n > 1 {
		rate = float64(n-1) / total.Seconds()
	}
	st := fmt.Sprintf("%d tokens · %.0f tok/s", n, rate)
	if thoughtFor > 0 {
		st += fmt.Sprintf(" · thought for %.1fs", thoughtFor.Seconds())
	}
	return answer.String(), st, nil
}
