package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const logName = "hum.log"

func pidPath() string { return pathIn("hum.pid") }
func logPath() string { return pathIn(logName) }

func readPID() (int, bool) {
	b, err := os.ReadFile(pidPath())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	// Signal 0 only checks the process exists and is ours.
	if err := syscall.Kill(pid, 0); err != nil {
		return pid, false
	}
	return pid, true
}

type health struct {
	Status string `json:"status"`
	Model  string `json:"model"`
	Addr   string `json:"addr"`
	PID    int    `json:"pid"`
	Uptime int    `json:"uptime_s"`
}

func probe(addr string, timeout time.Duration) (health, error) {
	var h health
	c := &http.Client{Timeout: timeout}
	resp, err := c.Get("http://" + addr + "/health")
	if err != nil {
		return h, err
	}
	defer resp.Body.Close()
	return h, json.NewDecoder(resp.Body).Decode(&h)
}

// startDaemon re-execs this binary as a detached `hum serve` child, then waits
// until the model has finished loading before returning control.
func startDaemon(cfg Config, wait time.Duration) error {
	if pid, alive := readPID(); alive {
		return fmt.Errorf("already running (pid %d) — use `hum restart`", pid)
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(humDir(), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "serve",
		"--model", cfg.Model, "--addr", cfg.Addr,
		"--python", cfg.Python, "--worker", cfg.Worker,
		"--cache-entries", strconv.Itoa(cfg.CacheEntries))
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survive this shell
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(pidPath(), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return err
	}
	_ = cmd.Process.Release()

	fmt.Printf("starting hum (pid %d), loading %s\n", pid, short(cfg.Model))
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return fmt.Errorf("worker died during startup — see %s", logPath())
		}
		if _, err := probe(cfg.Addr, 2*time.Second); err == nil {
			fmt.Printf("ready on http://%s  (logs: %s)\n", cfg.Addr, logPath())
			return nil
		}
		time.Sleep(400 * time.Millisecond)
	}
	return fmt.Errorf("timed out after %s waiting for the model — see %s", wait, logPath())
}

func stopDaemon(timeout time.Duration) error {
	pid, alive := readPID()
	if !alive {
		os.Remove(pidPath())
		return fmt.Errorf("not running")
	}
	// The child is a session leader; kill the group so the Python worker goes too.
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			os.Remove(pidPath())
			fmt.Printf("stopped (pid %d)\n", pid)
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	os.Remove(pidPath())
	fmt.Printf("stopped (pid %d, forced)\n", pid)
	return nil
}

func statusCmd(cfg Config) error {
	pid, alive := readPID()
	if !alive {
		fmt.Println("hum is not running")
		if cfg.Model != "" {
			fmt.Printf("  model  %s\n", short(cfg.Model))
			fmt.Printf("  addr   %s\n", cfg.Addr)
		}
		return nil
	}
	h, err := probe(cfg.Addr, 3*time.Second)
	if err != nil {
		fmt.Printf("hum is starting (pid %d) — model still loading\n", pid)
		fmt.Printf("  logs   %s\n", logPath())
		return nil
	}
	fmt.Println("hum is running")
	fmt.Printf("  pid    %d\n", h.PID)
	fmt.Printf("  addr   http://%s\n", h.Addr)
	fmt.Printf("  model  %s\n", short(h.Model))
	fmt.Printf("  uptime %s\n", dur(h.Uptime))
	fmt.Printf("  logs   %s\n", logPath())
	return nil
}

func logsCmd(follow bool, n int) error {
	f, err := os.Open(logPath())
	if err != nil {
		return fmt.Errorf("no log yet at %s", logPath())
	}
	defer f.Close()
	if !follow {
		b, _ := io.ReadAll(f)
		lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		if len(lines) > n {
			lines = lines[len(lines)-n:]
		}
		fmt.Println(strings.Join(lines, "\n"))
		return nil
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	for {
		k, err := f.Read(buf)
		if k > 0 {
			os.Stdout.Write(buf[:k])
		}
		if err == io.EOF {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
	}
}

func short(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

func dur(s int) string {
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm %ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh %dm", s/3600, (s%3600)/60)
	}
}
