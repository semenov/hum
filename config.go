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
}

func humDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".hum"
	}
	return filepath.Join(home, ".hum")
}

func pathIn(name string) string { return filepath.Join(humDir(), name) }

func defaultConfig() Config {
	python := "python3"
	worker := "worker.py"
	// The worker ships next to the binary.
	if exe, err := os.Executable(); err == nil {
		if p, err := filepath.EvalSymlinks(exe); err == nil {
			exe = p
		}
		worker = filepath.Join(filepath.Dir(exe), "worker.py")
	}
	return Config{Addr: "127.0.0.1:8090", Python: python, Worker: worker, CacheEntries: 4}
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
		return errors.New("no model set — run: hum start --model <path>")
	}
	if _, err := os.Stat(c.Model); err != nil {
		return fmt.Errorf("model not found: %s", c.Model)
	}
	if _, err := os.Stat(c.Worker); err != nil {
		return fmt.Errorf("worker not found: %s", c.Worker)
	}
	return nil
}
