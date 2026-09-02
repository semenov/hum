package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	MaxCtx int    `json:"max_context"`
}

func probe(addr string, timeout time.Duration) (health, error) {
	var h health
	c := &http.Client{Timeout: timeout}
	resp, err := c.Get("http://" + addr + "/health")
	if err != nil {
		return h, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return h, err
	}
	if resp.StatusCode != http.StatusOK {
		return h, fmt.Errorf("server reports: %s", h.Status)
	}
	return h, nil
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
	// Every setting goes on the command line, so the child does not depend on
	// what happened to be saved a moment ago.
	cmd := exec.Command(exe, "serve",
		"--model", cfg.Model, "--addr", cfg.Addr,
		"--python", cfg.Python, "--worker", cfg.Worker,
		"--cache-entries", strconv.Itoa(cfg.CacheEntries),
		"--cors="+strconv.FormatBool(cfg.CORS))
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

	u := newUI()
	sp := u.Spin("Loading " + prettyModel(cfg.Model))
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			sp.Stop()
			return fmt.Errorf("the worker died while starting — run `hum logs` to see why")
		}
		if _, err := probe(cfg.Addr, 2*time.Second); err == nil {
			sp.Stop()
			u.OK("Hum is ready.")
			u.Para("The server is listening on http://%s and speaks the OpenAI "+
				"chat completions API. Point OpenCode, your editor, or any OpenAI "+
				"SDK at it — no API key is required.", cfg.Addr)
			if cfg.CORS {
				u.Warn("Any website you visit can reach this server.")
				u.Para("That is what --cors allows, and it is what a browser app " +
					"needs. It also means a page you did not expect can send " +
					"prompts to your model. Turn it off with --cors=false.")
			}
			if host, _, _ := net.SplitHostPort(cfg.Addr); !isLoopback(host) {
				u.Warn("This is reachable from your network.")
				u.Para("There is no authentication, so anyone who can route to this "+
					"machine can use the model. Fine on a home network, not on a "+
					"cafe one. Others reach it at %s.", lanURL(cfg.Addr))
			}
			u.KV("Model", prettyModel(cfg.Model))
			u.KV("Process", strconv.Itoa(pid))
			u.KV("Logs", short(logPath()))
			u.Hint("Stop it again with", "hum stop")
			return nil
		}
		time.Sleep(400 * time.Millisecond)
	}
	sp.Stop()
	return fmt.Errorf("gave up after %s waiting for the model — run `hum logs` to see why", wait)
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
	u := newUI()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			os.Remove(pidPath())
			u.Off("Hum has stopped.")
			u.Para("The model has been unloaded and that memory is free again.")
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	os.Remove(pidPath())
	u.Off("Hum has stopped.")
	u.Para("It did not shut down cleanly in time, so it was forced. The model " +
		"has been unloaded either way.")
	return nil
}

func statusCmd(cfg Config) error {
	u := newUI()
	pid, alive := readPID()
	if !alive {
		u.Off("Hum is not running.")
		u.Para("No model is loaded, so none of your memory is being used.")
		if cfg.Model != "" {
			u.KV("Model", prettyModel(cfg.Model))
			u.KV("Address", cfg.Addr)
		}
		u.Hint("Start it with", "hum start")
		return nil
	}
	h, err := probe(cfg.Addr, 3*time.Second)
	if err != nil && strings.Contains(err.Error(), "server reports") {
		u.Fail("Hum is running but its worker has stopped.")
		u.Para("The server is shutting down; start it again once it has gone.")
		u.Hint("See why with", "hum logs -n 50")
		return nil
	}
	if err != nil {
		u.Off("Hum is still starting up.")
		u.Para("The process is alive but the model has not finished loading yet, " +
			"so it is not answering requests. This usually takes a few seconds.")
		u.KV("Process", strconv.Itoa(pid))
		u.Hint("Follow along with", "hum logs -f")
		return nil
	}
	u.OK("Hum is running.")
	u.Para("It has been up for %s and is serving on http://%s.", dur(h.Uptime), h.Addr)
	u.KV("Model", prettyModel(h.Model))
	if h.MaxCtx > 0 {
		u.KV("Context", fmt.Sprintf("%s tokens, measured against this Mac's memory",
			commas(h.MaxCtx)))
	}
	u.KV("Process", strconv.Itoa(h.PID))
	u.KV("Logs", short(logPath()))
	u.Hint("Stop it with", "hum stop")
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

// isLoopback reports whether an address is only reachable from this machine.
func isLoopback(host string) bool {
	if host == "" {
		return false // an empty host means every interface
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// lanURL rewrites a wildcard bind into an address other machines can actually
// type, so the warning is useful rather than just alarming.
func lanURL(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil &&
			!n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			return "http://" + net.JoinHostPort(n.IP.String(), port)
		}
	}
	return "http://" + addr
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
