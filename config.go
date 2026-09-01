package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Config is what `hum start` remembers, so later invocations need no flags.
type Config struct {
	Model        string `json:"model"`
	Addr         string `json:"addr"`
	Python       string `json:"python"`
	Worker       string `json:"worker"`
	CacheEntries int    `json:"cache_entries"`
	CORS         bool   `json:"cors"`
}

func humDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".hum"
	}
	return filepath.Join(home, ".hum")
}

func pathIn(name string) string { return filepath.Join(humDir(), name) }

// Set at build time so a package can point at the interpreter it installed:
//
//	go build -ldflags "-X main.builtinPython=/path/to/venv/bin/python
//	                   -X main.builtinWorker=/path/to/worker.py"
var (
	builtinPython string
	builtinWorker string
)

func defaultConfig() Config {
	python := "python3"
	worker := "worker.py"
	// The worker ships next to the binary when running from a checkout.
	if exe, err := os.Executable(); err == nil {
		if p, err := filepath.EvalSymlinks(exe); err == nil {
			exe = p
		}
		worker = filepath.Join(filepath.Dir(exe), "worker.py")
	}
	if builtinPython != "" {
		python = builtinPython
	}
	if builtinWorker != "" {
		worker = builtinWorker
	}
	// 4242 is unassigned by IANA and easy to remember, which matters more than
	// being unique — 1234 is LM Studio's and is surely taken elsewhere too.
	return Config{Addr: "127.0.0.1:4242", Python: python, Worker: worker, CacheEntries: 4}
}

func loadConfig() (Config, error) {
	cfg := defaultConfig()
	b, err := os.ReadFile(pathIn("config.json"))
	if err != nil {
		return cfg, err
	}
	saved := Config{}
	if err := json.Unmarshal(b, &saved); err != nil {
		return cfg, err
	}
	if saved.Model != "" {
		cfg.Model = saved.Model
	}
	if saved.Addr != "" {
		cfg.Addr = saved.Addr
	}
	if saved.Python != "" {
		cfg.Python = saved.Python
	}
	if saved.Worker != "" {
		cfg.Worker = saved.Worker
	}
	if saved.CacheEntries > 0 {
		cfg.CacheEntries = saved.CacheEntries
	}
	cfg.CORS = saved.CORS
	return cfg, nil
}

func saveConfig(cfg Config) error {
	if err := os.MkdirAll(humDir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(pathIn("config.json"), b, 0o644)
}

func (c Config) validate() error {
	if c.Model == "" {
		return errors.New("no model resolved")
	}
	if _, err := os.Stat(c.Model); err != nil {
		return fmt.Errorf("model not found: %s", c.Model)
	}
	if _, err := os.Stat(c.Worker); err != nil {
		return fmt.Errorf("worker not found: %s", c.Worker)
	}
	return nil
}

// resolveModel fills in Model, downloading the built-in pick on first run.
//
// explicit says whether --model was passed on this invocation. If it was, the
// path must exist and is used as given. Otherwise a remembered path is used
// when it is still there, and anything else means we fetch the catalogue pick
// again — a managed model that has been deleted should be replaced, not fatal.
func (c *Config) resolveModel(explicit bool) error {
	if explicit {
		if !haveModel(c.Model) {
			return fmt.Errorf("not a usable model directory: %s", c.Model)
		}
		return nil
	}
	// haveModel, not os.Stat: an interrupted download leaves the directory in
	// place with a .part file in it, and that must not read as ready.
	if c.Model != "" && haveModel(c.Model) {
		return nil
	}
	spec := pickModel()
	dir, err := EnsureModel(spec)
	if err != nil {
		return err
	}
	c.Model = dir
	return nil
}
