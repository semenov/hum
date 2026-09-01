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
		fmt.Print(helpText())

	default:
		fmt.Fprintf(os.Stderr, "hum: unknown command %q\nrun `hum help` to see what it can do\n", cmd)
		os.Exit(2)
	}
}
