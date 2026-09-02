package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// The agent's tools. Reading is unrestricted within the working directory;
// writing and running commands are gated, because a local model is wrong more
// often than a hosted one and the cost of a wrong edit is not symmetric.
const toolSchema = `[
 {"type":"function","function":{
   "name":"read_file","description":"Read a UTF-8 text file and return its contents.",
   "parameters":{"type":"object","properties":{
     "path":{"type":"string","description":"Path relative to the working directory."}},
     "required":["path"]}}},
 {"type":"function","function":{
   "name":"list_dir","description":"List the files and directories at a path.",
   "parameters":{"type":"object","properties":{
     "path":{"type":"string","description":"Path relative to the working directory. Defaults to '.'"}},
     "required":[]}}},
 {"type":"function","function":{
   "name":"search","description":"Search for a substring across files, returning matching lines with their file and line number.",
   "parameters":{"type":"object","properties":{
     "pattern":{"type":"string","description":"Text to look for."},
     "path":{"type":"string","description":"Directory to search. Defaults to '.'"}},
     "required":["pattern"]}}},
 {"type":"function","function":{
   "name":"write_file","description":"Create or overwrite a text file. Ask before using this if unsure.",
   "parameters":{"type":"object","properties":{
     "path":{"type":"string"},"content":{"type":"string"}},
     "required":["path","content"]}}},
 {"type":"function","function":{
   "name":"run_command","description":"Run a shell command and return its output. It starts in the working directory; it can read anywhere but cannot write outside it.",
   "parameters":{"type":"object","properties":{
     "command":{"type":"string"}},"required":["command"]}}}
]`

const agentSystem = `You are hum, a local coding assistant running on the user's own Mac.

You have tools for reading files, listing directories, searching text, writing
files and running shell commands. Use them rather than guessing: read a file
before describing it, and check a result after changing something.

Work in small steps. Prefer reading and searching over asking the user for
information you could look up yourself. When the task is done, say what you did
in a sentence or two — do not repeat the whole file back.`

const (
	maxToolOutput = 12000 // characters returned to the model per call
	maxSteps      = 24    // hard stop on a runaway loop
)

// destructive reports whether a tool changes the world.
func destructive(name string) bool { return name == "write_file" || name == "run_command" }

type agentOpts struct {
	root        string
	allowWrite  bool // may create and overwrite files, inside root only
	allowShell  bool // may run commands — NOT confined to root, see toolsFor
	interactive bool
	in          *bufio.Reader
}

// toolsFor offers only the tools that are actually permitted. Advertising a
// tool and then refusing it wastes the model's steps and confuses it.
func toolsFor(o agentOpts) json.RawMessage {
	var all []json.RawMessage
	if err := json.Unmarshal([]byte(toolSchema), &all); err != nil {
		return json.RawMessage(toolSchema)
	}
	keep := all[:0]
	for _, t := range all {
		var d ToolDef
		json.Unmarshal(t, &d)
		switch d.Function.Name {
		case "write_file":
			if !o.interactive && !o.allowWrite {
				continue
			}
		case "run_command":
			if !o.interactive && !o.allowShell {
				continue
			}
		}
		keep = append(keep, t)
	}
	b, _ := json.Marshal(keep)
	return b
}

// confirm gates a destructive call. With no terminal there is nobody to ask, so
// permission has to have been granted on the command line.
func (o agentOpts) confirm(u *UI, kind, what, detail string) bool {
	if !o.interactive {
		return (kind == "write" && o.allowWrite) || (kind == "shell" && o.allowShell)
	}
	if (kind == "write" && o.allowWrite) || (kind == "shell" && o.allowShell) {
		return true
	}
	fmt.Printf("\n    %s %s\n", u.p.rgb(255, 180, 90, "!"), u.p.bold(what))
	for _, line := range strings.Split(strings.TrimRight(detail, "\n"), "\n") {
		fmt.Printf("      %s\n", u.p.dim(line))
	}
	fmt.Printf("\n    %s %s ", "Allow this?", u.p.dim("[y/N]"))
	line, err := o.in.ReadString('\n')
	if err != nil {
		return false
	}
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

// safePath keeps the agent inside the directory it was started in. Symlinks
// are resolved first, otherwise a link inside the directory that points
// outside it would let write_file modify a file the sandbox promise covers.
func safePath(root, p string) (string, error) {
	if p == "" {
		p = "."
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, p)
	}
	abs = filepath.Clean(abs)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	real, err := resolveExisting(abs)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(realRoot, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the working directory", p)
	}
	return real, nil
}

// resolveExisting resolves symlinks in as much of p as exists, so a path to a
// file that is about to be created is judged by the directory it lands in.
func resolveExisting(p string) (string, error) {
	var tail []string
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				real = filepath.Join(real, tail[i])
			}
			return real, nil
		}
		dir, base := filepath.Split(filepath.Clean(p))
		if dir == "" || dir == p {
			return "", fmt.Errorf("cannot resolve %s", p)
		}
		tail = append(tail, base)
		p = filepath.Clean(dir)
	}
}

func clip(s string) string {
	if len(s) <= maxToolOutput {
		return s
	}
	return s[:maxToolOutput] + fmt.Sprintf("\n… truncated, %d more characters", len(s)-maxToolOutput)
}

// runTool executes one call and returns what the model should see.
func runTool(u *UI, o agentOpts, name, args string) string {
	var a map[string]any
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return "error: arguments were not valid JSON"
	}
	str := func(k string) string {
		if v, ok := a[k].(string); ok {
			return v
		}
		return ""
	}

	switch name {
	case "read_file":
		p, err := safePath(o.root, str("path"))
		if err != nil {
			return "error: " + err.Error()
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "error: " + err.Error()
		}
		return clip(string(b))

	case "list_dir":
		p, err := safePath(o.root, str("path"))
		if err != nil {
			return "error: " + err.Error()
		}
		ents, err := os.ReadDir(p)
		if err != nil {
			return "error: " + err.Error()
		}
		var out []string
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if e.IsDir() {
				out = append(out, e.Name()+"/")
			} else {
				out = append(out, e.Name())
			}
		}
		sort.Strings(out)
		if len(out) == 0 {
			return "(empty)"
		}
		return clip(strings.Join(out, "\n"))

	case "search":
		p, err := safePath(o.root, str("path"))
		if err != nil {
			return "error: " + err.Error()
		}
		return clip(searchTree(p, str("pattern"), o.root))

	case "write_file":
		p, err := safePath(o.root, str("path"))
		if err != nil {
			return "error: " + err.Error()
		}
		content := str("content")
		rel, _ := filepath.Rel(o.root, p)
		verb := "Create"
		if _, err := os.Stat(p); err == nil {
			verb = "Overwrite"
		}
		if !o.confirm(u, "write", fmt.Sprintf("%s %s (%d bytes)", verb, rel, len(content)), preview(content)) {
			return "error: the user declined this write"
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "error: " + err.Error()
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return "error: " + err.Error()
		}
		return fmt.Sprintf("wrote %d bytes to %s", len(content), rel)

	case "run_command":
		cmd := str("command")
		label := "Run a shell command"
		if !sandboxAvailable() {
			label += " (writes are NOT confined — no sandbox on this system)"
		}
		if !o.confirm(u, "shell", label, cmd) {
			return "error: the user declined to run this command"
		}
		c := sandboxedShell(cmd, o.root)
		c.Dir = o.root
		out, err := c.CombinedOutput()
		res := string(out)
		if err != nil {
			res += "\n(exit: " + err.Error() + ")"
		}
		if strings.TrimSpace(res) == "" {
			res = "(no output)"
		}
		return clip(res)
	}
	return "error: no such tool"
}

// Seatbelt confines writes to the working directory at the kernel level, so a
// command cannot escape the way `printf > /tmp/x` did before this existed —
// nor through a symlink, a rename, or a different language's file API.
//
// Reads and network are deliberately left alone: a confined read set breaks
// almost every real command (interpreters, compilers and git all read far
// outside the project), and the damage that matters is modification.
func sandboxAvailable() bool {
	_, err := os.Stat("/usr/bin/sandbox-exec")
	return err == nil
}

func sandboxedShell(cmd, root string) *exec.Cmd {
	real, err := filepath.EvalSymlinks(root)
	if err != nil || !sandboxAvailable() {
		return exec.Command("sh", "-c", cmd)
	}
	// The path must be the resolved one: on macOS /tmp is a link to /private/tmp
	// and a rule naming the link never matches.
	policy := fmt.Sprintf(`(version 1)
(allow default)
(deny file-write* (subpath "/"))
(allow file-write* (subpath %q))
(allow file-write-data
  (literal "/dev/null") (literal "/dev/stdout") (literal "/dev/stderr")
  (literal "/dev/tty") (literal "/dev/dtracehelper"))`, real)
	return exec.Command("/usr/bin/sandbox-exec", "-p", policy, "sh", "-c", cmd)
}

func preview(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > 12 {
		lines = append(lines[:12], fmt.Sprintf("… %d more lines", len(s)-12))
	}
	return strings.Join(lines, "\n")
}

// searchTree is a small grep: substring match, skipping binaries and vendor dirs.
func searchTree(root, pattern, base string) string {
	if pattern == "" {
		return "error: no pattern given"
	}
	var hits []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || len(hits) > 200 {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", ".venv", "__pycache__":
				return filepath.SkipDir
			}
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil || len(b) > 1<<20 {
			return nil
		}
		s := string(b)
		if strings.IndexByte(s, 0) >= 0 {
			return nil // binary
		}
		rel, _ := filepath.Rel(base, p)
		for i, line := range strings.Split(s, "\n") {
			if strings.Contains(line, pattern) {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", rel, i+1, strings.TrimSpace(line)))
				if len(hits) > 200 {
					break
				}
			}
		}
		return nil
	})
	if len(hits) == 0 {
		return "no matches"
	}
	return strings.Join(hits, "\n")
}
