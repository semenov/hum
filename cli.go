package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

const usage = `hum — a fast local LLM server for Apple Silicon

usage: hum <command> [flags]

commands:
  start      start the server in the background and return
             (first run downloads the model — no setup, no model picking)
  stop       stop the server
  restart    stop then start
  status     show whether it is running, and where
  logs       show the log  (-f to follow, -n to set line count)
  serve      run in the foreground (what start launches; useful for debugging)
  model      show which model was picked for this machine, and why
  config     print the saved configuration
  version    print the version

flags for start/serve/restart:
  --model PATH        override the built-in model choice
  --addr HOST:PORT    listen address              (default 127.0.0.1:8090)
  --python PATH       python interpreter for the worker
  --worker PATH       worker.py location
  --cache-entries N   conversations kept in the prompt cache   (default 4)
  --wait DURATION     how long start waits for the model       (default 3m)

examples:
  hum start
  hum status
  hum logs -f
  hum stop
`

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
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	cfg, _ := loadConfig() // absent config is fine; defaults apply

	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "hum: "+err.Error())
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
		spec := pickModel()
		dir := modelDir(spec.Repo)
		state := "not downloaded"
		if haveModel(dir) {
			state = "ready"
		}
		size := ""
		if spec.Bytes > 0 {
			size = " (" + humanBytes(spec.Bytes) + ")"
		}
		fmt.Printf("system memory  %s\nselected       %s%s\nrepo           %s\nlocation       %s\nstatus         %s\n",
			humanBytes(systemRAM()), spec.Name, size, spec.Repo, short(dir), state)

	case "config":
		fmt.Printf("model         %s\naddr          %s\npython        %s\nworker        %s\ncache-entries %d\nconfig file   %s\n",
			short(cfg.Model), cfg.Addr, short(cfg.Python), short(cfg.Worker),
			cfg.CacheEntries, short(pathIn("config.json")))

	case "version", "-v", "--version":
		fmt.Println("hum " + version)

	case "help", "-h", "--help":
		fmt.Print(usage)

	default:
		fmt.Fprintf(os.Stderr, "hum: unknown command %q\n\n", cmd)
		fmt.Print(usage)
		os.Exit(2)
	}
}
