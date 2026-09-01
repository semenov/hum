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
		u.Head("MODEL", "")
		u.Para("There is nothing to choose. Hum reads how much unified memory this " +
			"Mac has and runs the largest model that fits comfortably, because the " +
			"weights and the cache both have to stay under the wired-memory limit.")
		u.KV("This Mac", humanBytes(systemRAM())+" of unified memory")
		u.KV("Model", spec.Name)
		if spec.Bytes > 0 {
			u.KV("Download", humanBytes(spec.Bytes))
		}
		u.KV("Source", spec.Repo)
		u.KV("Stored in", short(dir))
		switch {
		case haveModel(dir):
			u.KV("Status", "Downloaded and ready")
		case haveModel(cfg.Model):
			u.KV("Status", "Using an existing copy at "+short(cfg.Model))
		default:
			u.KV("Status", "Not downloaded yet")
			u.Hint("Fetch it with", "hum start")
		}
		u.Blank()

	case "config":
		u := newUI()
		u.Head("CONFIG", "")
		u.Para("These settings are remembered in %s and reused on every run. Pass "+
			"a flag to any command to change one.", short(pathIn("config.json")))
		model := short(cfg.Model)
		if model == "" {
			model = "not chosen yet — will be picked on first start"
		}
		u.KV("Model", model)
		u.KV("Address", cfg.Addr)
		u.KV("Python", short(cfg.Python))
		u.KV("Worker", short(cfg.Worker))
		u.KV("Cache", fmt.Sprintf("%d conversations kept warm", cfg.CacheEntries))
		u.Blank()

	case "version", "-v", "--version":
		u := newUI()
		u.Head("hum "+version, "")
		u.Para("A fast local LLM server for Apple Silicon, speaking the OpenAI " +
			"chat completions API.")

	case "help", "-h", "--help":
		fmt.Print(helpText())

	default:
		u := newUI()
		u.Fail("There is no command called %q.", cmd)
		u.Hint("See everything it can do with", "hum help")
		u.Blank()
		os.Exit(2)
	}
}
