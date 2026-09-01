package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// UI keeps command output looking like one program. Everything here degrades to
// plain text when stdout is not a terminal or NO_COLOR is set; see palette.
type UI struct {
	p palette
	// blank tracks whether the last thing written ended with an empty line, so
	// blocks can be separated without ever doubling up the gap.
	blank bool
}

func newUI() *UI { return &UI{p: newPalette()} }

// gap writes a separating blank line unless one is already there.
func (u *UI) gap() {
	if !u.blank {
		fmt.Println()
		u.blank = true
	}
}

// Running, stopped and failed states, so they are recognisable at a glance.
func (u *UI) OK(format string, a ...any) {
	fmt.Printf("\n  %s %s\n\n", u.p.green("●"), u.p.bold(fmt.Sprintf(format, a...)))
	u.blank = true
}

func (u *UI) Off(format string, a ...any) {
	fmt.Printf("\n  %s %s\n\n", u.p.dim("○"), u.p.bold(fmt.Sprintf(format, a...)))
	u.blank = true
}

func (u *UI) Fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "\n  %s %s\n\n", u.p.rgb(255, 110, 110, "✗"),
		u.p.bold(fmt.Sprintf(format, a...)))
	u.blank = true
}

// Head labels a block of key/value lines.
func (u *UI) Head(title, note string) {
	line := "\n  " + u.p.green(title)
	if note != "" {
		line += u.p.dim("  " + note)
	}
	fmt.Println(line + "\n")
	u.blank = true
}

const keyWidth = 12

// Para prints an explanatory paragraph, wrapped and indented under a heading.
// Output is meant to be read, not grepped, so it gets room to breathe.
func (u *UI) Para(format string, a ...any) {
	const width = 68
	words := strings.Fields(fmt.Sprintf(format, a...))
	line := ""
	for _, w := range words {
		if line != "" && len(line)+1+len(w) > width {
			fmt.Println("    " + u.p.dim(line))
			line = ""
		}
		if line != "" {
			line += " "
		}
		line += w
	}
	if line != "" {
		fmt.Println("    " + u.p.dim(line))
	}
	fmt.Println()
	u.blank = true
}

// KV prints one aligned label/value pair. Values are blue: they are the part
// worth copying.
func (u *UI) KV(key, value string) {
	pad := keyWidth - len(key)
	if pad < 1 {
		pad = 1
	}
	fmt.Printf("    %s%s%s\n", u.p.dim(key), strings.Repeat(" ", pad), u.p.blue(value))
	u.blank = false
}

// Hint is a suggested next command.
func (u *UI) Hint(text, cmd string) {
	u.gap()
	fmt.Printf("    %s %s\n\n", u.p.dim(text), u.p.blue(cmd))
	u.blank = true
}

func (u *UI) Blank() { u.gap() }

// Spinner shows progress for work with no measurable percentage — loading a
// model, mostly. Silent when not on a terminal.
type Spinner struct {
	p     palette
	label string
	stop  chan struct{}
	wg    sync.WaitGroup
}

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (u *UI) Spin(label string) *Spinner {
	s := &Spinner{p: u.p, label: label, stop: make(chan struct{})}
	if !u.p.on {
		fmt.Printf("\n  %s\n", label)
		return s
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(90 * time.Millisecond)
		defer t.Stop()
		start := time.Now()
		for i := 0; ; i++ {
			select {
			case <-s.stop:
				return
			case <-t.C:
				fmt.Printf("\r\033[K  %s %s %s",
					s.p.blue(spinFrames[i%len(spinFrames)]), label,
					s.p.dim(fmt.Sprintf("%ds", int(time.Since(start).Seconds()))))
			}
		}
	}()
	return s
}

func (s *Spinner) Stop() {
	if !s.p.on {
		return
	}
	close(s.stop)
	s.wg.Wait()
	fmt.Print("\r\033[K")
}
