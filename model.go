package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// A ModelSpec is a model hum will run without being asked. Which one depends on
// how much unified memory the machine has: weights plus the KV cache have to
// stay under the wired limit, which macOS puts at roughly 75% of RAM.
type ModelSpec struct {
	Repo   string
	Name   string
	Bytes  int64
	MinRAM int64 // bytes of system memory required to pick this tier
}

const gb = 1 << 30

// Ordered from largest to smallest; the first that fits wins.
var Catalog = []ModelSpec{
	{"lmstudio-community/Qwen3.6-35B-A3B-MLX-4bit", "Qwen3.6 35B-A3B", 21 * gb, 32 * gb},
	{"mlx-community/Qwen3-14B-4bit", "Qwen3 14B", 9 * gb, 24 * gb},
	{"mlx-community/Qwen3-8B-4bit", "Qwen3 8B", 5 * gb, 16 * gb},
	{"mlx-community/Qwen3-4B-4bit", "Qwen3 4B", 3 * gb, 8 * gb},
	{"mlx-community/Qwen3-0.6B-4bit", "Qwen3 0.6B", 1 * gb, 0},
}

func systemRAM() int64 {
	s, err := syscall.Sysctl("hw.memsize")
	if err != nil {
		return 0
	}
	// Sysctl returns raw bytes in a string, and trims a trailing NUL.
	b := make([]byte, 8)
	copy(b, s)
	return int64(binary.LittleEndian.Uint64(b))
}

func pickModel() ModelSpec {
	// Escape hatch: point hum at any MLX repo without touching the catalogue.
	if r := os.Getenv("HUM_MODEL_REPO"); r != "" {
		return ModelSpec{Repo: r, Name: r}
	}
	ram := systemRAM()
	for _, m := range Catalog {
		if ram >= m.MinRAM {
			return m
		}
	}
	return Catalog[len(Catalog)-1]
}

func modelDir(repo string) string {
	return filepath.Join(humDir(), "models", strings.ReplaceAll(repo, "/", "__"))
}

// modelLabel turns a directory back into something worth reading. Managed
// models live in a flattened directory name; anything else is shown as its
// own basename.
func modelLabel(path string) string {
	base := filepath.Base(path)
	if strings.Contains(base, "__") {
		return strings.ReplaceAll(base, "__", "/")
	}
	return base
}

// haveModel reports whether a directory looks like a usable MLX model.
func haveModel(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		return false
	}
	m, _ := filepath.Glob(filepath.Join(dir, "*.safetensors"))
	return len(m) > 0
}

type hfFile struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	LFS  *struct {
		Size int64 `json:"size"`
	} `json:"lfs"`
}

func (f hfFile) size() int64 {
	if f.LFS != nil && f.LFS.Size > 0 {
		return f.LFS.Size
	}
	return f.Size
}

// wanted skips repo furniture that the runtime never reads.
func wanted(path string) bool {
	base := filepath.Base(path)
	if strings.HasPrefix(base, ".") {
		return false
	}
	switch strings.ToLower(filepath.Ext(base)) {
	case ".md", ".png", ".jpg", ".jpeg", ".gif", ".svg":
		return false
	}
	return strings.ToUpper(base) != "LICENSE"
}

func listRepo(repo string) ([]hfFile, error) {
	url := "https://huggingface.co/api/models/" + repo + "/tree/main?recursive=1"
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("hugging face returned %s for %s", resp.Status, repo)
	}
	var all []hfFile
	if err := json.NewDecoder(resp.Body).Decode(&all); err != nil {
		return nil, err
	}
	var out []hfFile
	for _, f := range all {
		if f.Type == "file" && wanted(f.Path) {
			out = append(out, f)
		}
	}
	return out, nil
}

// EnsureModel downloads the model unless it is already on disk, and returns the
// directory to hand to the worker.
func EnsureModel(spec ModelSpec) (string, error) {
	dir := modelDir(spec.Repo)
	if haveModel(dir) {
		return dir, nil
	}
	files, err := listRepo(spec.Repo)
	if err != nil {
		return "", err
	}

	var total, already int64
	for _, f := range files {
		total += f.size()
		if st, err := os.Stat(filepath.Join(dir, f.Path)); err == nil && st.Size() == f.size() {
			already += f.size()
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	u := newUI()
	u.Head("FIRST RUN", "the model needs to be downloaded once")
	if os.Getenv("HUM_MODEL_REPO") != "" {
		u.Para("Fetching %s because HUM_MODEL_REPO asks for it, rather than the "+
			"model Hum would pick for this Mac.", spec.Repo)
	} else {
		u.Para("Hum chose %s for this Mac. It has %s of unified memory, and this "+
			"is the largest model that comfortably fits alongside its cache.",
			spec.Name, humanBytes(systemRAM()))
	}
	u.Para("The download is %s and happens only this once. It resumes where it "+
		"left off if interrupted, so it is safe to stop and come back later.",
		humanBytes(total))
	u.Para("Now is a good moment to step away — nothing else is needed from you, " +
		"and hum will start serving the moment this finishes.")
	u.Para("What you get at the end is worth the wait: this model runs entirely " +
		"on your own Mac. No account, no API key, no per-token bill, no rate " +
		"limits, and nothing you type ever leaves the machine. It keeps working " +
		"on a plane, and it will still work in ten years.")
	p := NewProgress(total)
	p.Add(already)
	p.Start()
	defer p.Stop()

	for _, f := range files {
		dst := filepath.Join(dir, f.Path)
		if st, err := os.Stat(dst); err == nil && st.Size() == f.size() {
			continue // already have it
		}
		if err := downloadFile(spec.Repo, f, dst, p); err != nil {
			return "", fmt.Errorf("downloading %s: %w", f.Path, err)
		}
	}
	if !haveModel(dir) {
		return "", fmt.Errorf("the download finished but %s does not contain a usable model", short(dir))
	}
	return dir, nil
}

// downloadFile fetches one file, resuming a partial .part from where it
// stopped. Whole-file restarts are unacceptable when the model is 20 GB and
// the connection is a cafe.
func downloadFile(repo string, f hfFile, dst string, p *Progress) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".part"

	var have int64
	if st, err := os.Stat(tmp); err == nil && st.Size() < f.size() {
		have = st.Size()
	}

	req, err := http.NewRequest("GET", "https://huggingface.co/"+repo+"/resolve/main/"+f.Path, nil)
	if err != nil {
		return err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusPartialContent:
		flags |= os.O_APPEND
		p.Add(have) // count what a previous run already fetched
	case http.StatusOK:
		flags |= os.O_TRUNC // server ignored the range; start over
		have = 0
	default:
		return fmt.Errorf("%s", resp.Status)
	}

	out, err := os.OpenFile(tmp, flags, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(io.MultiWriter(out, p), resp.Body)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err // leave .part in place so the next run can resume it
	}
	return os.Rename(tmp, dst)
}

var _ = time.Second
