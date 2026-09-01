package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"strings"
)

// agentCmd is chat with hands: the same REPL, but the model can read, search,
// write and run things, and every change is confirmed before it happens.
func agentCmd(cfg Config, allowWrite, allowShell bool) error {
	u := newUI()
	if err := requireServer(u, cfg); err != nil {
		return err
	}
	h, _ := probe(cfg.Addr, 5000000000)
	root, err := os.Getwd()
	if err != nil {
		return err
	}

	in := bufio.NewReader(os.Stdin)
	o := agentOpts{root: root, allowWrite: allowWrite, allowShell: allowShell, interactive: true, in: in}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		fmt.Printf("\n\n  %s\n\n", u.p.dim("Left the agent. The server is still running."))
		os.Exit(0)
	}()

	u.OK("Agent · %s", prettyModel(h.Model))
	u.Para("It reads and searches inside %s, and asks before it writes anything. "+
		"It can also run shell commands, which start there but are not confined "+
		"to it — read those before allowing them.", short(root))
	if allowWrite || allowShell {
		u.Para("Some permissions were granted on the command line, so it will not " +
			"ask for those. Watch what it does.")
	}
	u.Para("/reset starts over, /exit or Ctrl-D leaves.")

	msgs := []turn{{"role": "system", "content": agentSystem}}
	for {
		fmt.Printf("  %s ", u.p.blue("you ›"))
		line, err := in.ReadString('\n')
		if err != nil {
			fmt.Printf("\n\n  %s\n\n", u.p.dim("Left the agent. The server is still running."))
			return nil
		}
		text := strings.TrimSpace(line)
		switch {
		case text == "":
			continue
		case text == "/exit" || text == "/quit":
			fmt.Printf("\n  %s\n\n", u.p.dim("Left the agent. The server is still running."))
			return nil
		case text == "/reset":
			msgs = msgs[:1]
			fmt.Printf("  %s\n\n", u.p.dim("(conversation cleared)"))
			continue
		case strings.HasPrefix(text, "/"):
			fmt.Printf("  %s\n\n", u.p.dim("commands: /reset, /exit"))
			continue
		}

		msgs = append(msgs, turn{"role": "user", "content": text})
		fmt.Println()
		next, answer, err := agentLoop(u, cfg.Addr, msgs, o, true)
		if err != nil {
			u.Fail("%s", err)
			msgs = msgs[:len(msgs)-1]
			continue
		}
		msgs = next
		wrap := newWrapper(8)
		fmt.Printf("\n  %s ", u.p.green("hum ›"))
		wrap.Write(strings.TrimSpace(answer))
		wrap.Flush()
		fmt.Print("\n\n")
	}
}
