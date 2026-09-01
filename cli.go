package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

var version = "0.1.0"

// bindServerFlags layers CLI flags over the saved config.
func bindServerFlags(fs *flag.FlagSet, cfg *Config) *time.Duration {
	fs.StringVar(&cfg.Model, "model", cfg.Model, "model directory")
	fs.StringVar(&cfg.Addr, "addr", cfg.Addr, "listen address")
	fs.StringVar(&cfg.Python, "python", cfg.Python, "python interpreter")
	fs.StringVar(&cfg.Worker, "worker", cfg.Worker, "worker.py location")
	fs.IntVar(&cfg.CacheEntries, "cache-entries", cfg.CacheEntries, "cached conversations")
	return fs.Duration("wait", 3*time.Minute, "how long to wait for the model")
}

func main() {
	if len(os.Args) < 2 {
		fmt.Print(helpText())
		return
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	cfg, _ := loadConfig() // absent config is fine; defaults apply

	fail := func(err error) {
		newUI().Fail("%s", err.Error())
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}

	switch cmd {
	case "start", "serve", "restart":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		wait := bindServerFlags(fs, &cfg)
		if err := fs.Parse(args); err != nil {
			fail(err)
		}
		explicit := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "model" {
				explicit = true
			}
		})
		if err := cfg.resolveModel(explicit); err != nil {
			fail(err)
		}
		if err := cfg.validate(); err != nil {
			fail(err)
		}
		if err := saveConfig(cfg); err != nil {
			fail(err)
		}
		switch cmd {
		case "serve":
			if err := runServer(cfg); err != nil {
				fail(err)
			}
		case "restart":
			if _, alive := readPID(); alive {
				if err := stopDaemon(10 * time.Second); err != nil {
					fail(err)
				}
			}
			if err := startDaemon(cfg, *wait); err != nil {
				fail(err)
			}
		default:
			if err := startDaemon(cfg, *wait); err != nil {
				fail(err)
			}
		}

	case "stop":
		if err := stopDaemon(10 * time.Second); err != nil {
			fail(err)
		}

	case "status":
		if err := statusCmd(cfg); err != nil {
			fail(err)
		}

	case "logs":
		fs := flag.NewFlagSet("logs", flag.ExitOnError)
		follow := fs.Bool("f", false, "follow the log")
		n := fs.Int("n", 40, "lines to show")
		if err := fs.Parse(args); err != nil {
			fail(err)
		}
		if err := logsCmd(*follow, *n); err != nil {
			fail(err)
		}

	case "model":
		u := newUI()
		spec := pickModel()
		dir := modelDir(spec.Repo)
		u.Head("MODEL", "chosen for this machine")
		u.KV("memory", humanBytes(systemRAM())+" unified")
		u.KV("selected", spec.Name)
		if spec.Bytes > 0 {
			u.KV("download", humanBytes(spec.Bytes))
		}
		u.KV("repo", spec.Repo)
		u.KV("path", short(dir))
		if haveModel(dir) {
			u.KV("status", "ready")
		} else if haveModel(cfg.Model) {
			u.KV("status", "using "+short(cfg.Model))
		} else {
			u.KV("status", "not downloaded yet")
			u.Hint("fetch it with", "hum start")
		}
		u.Blank()

	case "config":
		u := newUI()
		u.Head("CONFIG", short(pathIn("config.json")))
		u.KV("model", short(cfg.Model))
		u.KV("address", cfg.Addr)
		u.KV("python", short(cfg.Python))
		u.KV("worker", short(cfg.Worker))
		u.KV("cache", fmt.Sprintf("%d conversations", cfg.CacheEntries))
		u.Blank()

	case "version", "-v", "--version":
		u := newUI()
		u.Head("hum "+version, "a fast local LLM server for Apple Silicon")
		u.Blank()

	case "help", "-h", "--help":
		fmt.Print(helpText())

	default:
		u := newUI()
		u.Fail("unknown command %q", cmd)
		u.Hint("see what it can do with", "hum help")
		u.Blank()
		os.Exit(2)
	}
}
