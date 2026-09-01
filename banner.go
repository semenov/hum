package main

import (
	"fmt"
	"os"
	"strings"
)

// ---- colour ---------------------------------------------------------------

type palette struct{ on, truecolor bool }

func newPalette() palette {
	st, _ := os.Stdout.Stat()
	tty := st != nil && st.Mode()&os.ModeCharDevice != 0
	// NO_COLOR is a de-facto standard; TERM=dumb means no escapes at all.
	if !tty || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return palette{}
	}
	ct := os.Getenv("COLORTERM")
	return palette{on: true, truecolor: ct == "truecolor" || ct == "24bit"}
}

func (p palette) rgb(r, g, b int, s string) string {
	if !p.on {
		return s
	}
	if !p.truecolor {
		// Approximate: anything greener than it is blue becomes green.
		if g > b {
			return "\033[92m" + s + "\033[0m"
		}
		return "\033[94m" + s + "\033[0m"
	}
	return fmt.Sprintf("\033[38;2;%d;%d;%dm%s\033[0m", r, g, b, s)
}

func (p palette) blue(s string) string  { return p.rgb(78, 168, 255, s) }
func (p palette) green(s string) string { return p.rgb(61, 220, 132, s) }
func (p palette) dim(s string) string {
	if !p.on {
		return s
	}
	return "\033[2m" + s + "\033[0m"
}
func (p palette) bold(s string) string {
	if !p.on {
		return s
	}
	return "\033[1m" + s + "\033[0m"
}

// fade colours a string left to right, blue into green.
func (p palette) fade(s string) string {
	if !p.on {
		return s
	}
	runes := []rune(s)
	n := len(runes)
	var b strings.Builder
	for i, r := range runes {
		if r == ' ' {
			b.WriteRune(r)
			continue
		}
		t := 0.0
		if n > 1 {
			t = float64(i) / float64(n-1)
		}
		b.WriteString(p.rgb(
			int(78+(61-78)*t),
			int(168+(220-168)*t),
			int(255+(132-255)*t),
			string(r)))
	}
	return b.String()
}

// ---- art ------------------------------------------------------------------

var wordArt = []string{
	"██   ██  ██    ██  ███    ███",
	"██   ██  ██    ██  ████  ████",
	"███████  ██    ██  ██ ████ ██",
	"██   ██  ██    ██  ██  ██  ██",
	"██   ██  ████████  ██      ██",
}

func banner(p palette) string {
	var b strings.Builder
	b.WriteString("\n")
	for _, line := range wordArt {
		b.WriteString("  ")
		b.WriteString(p.fade(line))
		b.WriteString("\n")
	}
	return b.String()
}

// ---- help -----------------------------------------------------------------

type cmdDoc struct{ name, desc string }

var commands = []cmdDoc{
	{"start", "download the model if needed, then serve in the background"},
	{"stop", "stop the server"},
	{"restart", "stop, then start again"},
	{"status", "is it running, on what address, with which model"},
	{"logs", "show the log  (-f to follow, -n for line count)"},
	{"model", "which model was picked for this machine, and why"},
	{"config", "the saved configuration"},
	{"serve", "run in the foreground, for debugging"},
	{"version", "print the version"},
}

var flagsDoc = []cmdDoc{
	{"--model PATH", "use a specific model instead of the built-in choice"},
	{"--addr HOST:PORT", "listen somewhere other than 127.0.0.1:8090"},
	{"--python PATH", "python interpreter that runs the worker"},
	{"--cache-entries N", "conversations kept warm in the prompt cache"},
}

func helpText() string {
	p := newPalette()
	var b strings.Builder
	b.WriteString(banner(p))
	b.WriteString("\n  " + p.bold("A fast local LLM server for Apple Silicon.") + "\n")
	b.WriteString("  " + p.dim("OpenAI-compatible. Nothing to configure, no model to choose.") + "\n\n")

	b.WriteString("  " + p.green("QUICK START") + "\n")
	b.WriteString("    " + p.blue("hum start") + "\n")
	b.WriteString("    " + p.dim("Downloads the right model for your Mac the first time, then serves") + "\n")
	b.WriteString("    " + p.dim("on http://127.0.0.1:8090/v1 — point any OpenAI client at it.") + "\n\n")

	b.WriteString("  " + p.green("COMMANDS") + "\n")
	for _, c := range commands {
		b.WriteString(fmt.Sprintf("    %s%s%s\n",
			p.blue(c.name), strings.Repeat(" ", 12-len(c.name)), p.dim(c.desc)))
	}
	b.WriteString("\n  " + p.green("OPTIONS") + p.dim("  (for start, restart and serve)") + "\n")
	for _, f := range flagsDoc {
		b.WriteString(fmt.Sprintf("    %s%s%s\n",
			p.blue(f.name), strings.Repeat(" ", 20-len(f.name)), p.dim(f.desc)))
	}
	b.WriteString("\n  " + p.green("EXAMPLES") + "\n")
	for _, e := range [][2]string{
		{"hum start", "first run downloads the model, later runs are instant"},
		{"hum status", "check it is up"},
		{"hum logs -f", "watch what it is doing"},
		{"hum stop", "shut it down and free the memory"},
	} {
		b.WriteString(fmt.Sprintf("    %s%s%s\n",
			p.blue(e[0]), strings.Repeat(" ", 16-len(e[0])), p.dim("# "+e[1])))
	}
	b.WriteString("\n  " + p.dim("docs: https://github.com/semenov/hum") + "\n\n")
	return b.String()
}
