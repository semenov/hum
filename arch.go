package main

import (
	"runtime"
	"syscall"
)

// underRosetta reports whether this process is an x86_64 binary being
// translated on an Apple Silicon Mac.
func underRosetta() bool {
	v, err := syscall.SysctlUint32("sysctl.proc_translated")
	return err == nil && v == 1
}

// archProblem returns the headline and explanation for an unsupported machine,
// or empty strings when everything is fine. Split out from the check itself so
// every branch can be tested without the hardware.
func archProblem(goos, goarch string, rosetta bool) (headline, detail string) {
	if goos != "darwin" {
		return "hum only runs on a Mac.",
			"It serves models through MLX, which is Apple's framework and speaks " +
				"only to Apple's GPUs. This machine reports " + goos + "/" + goarch +
				". On anything else, llama.cpp is the usual answer."
	}
	if rosetta {
		return "This build is being translated by Rosetta.",
			"The binary is x86_64 but the Mac underneath is Apple Silicon, and a " +
				"translated process cannot reach the GPU. The native build will be " +
				"fine — reinstall it with `arch -arm64 brew reinstall hum`."
	}
	if goarch != "arm64" {
		return "This Mac is never going to hum.",
			"It has an Intel chip, and hum runs models on the GPU through MLX, " +
				"which exists only for Apple Silicon. Nothing here is on the other " +
				"end of that conversation. An Intel Mac can still run a local " +
				"model — llama.cpp is the usual answer — just not this way."
	}
	return "", ""
}

// memoryProblem returns the headline and explanation for a Mac with the right
// chip but not enough memory, or empty strings when there is room. Memory it
// could not read is not a reason to refuse to start.
func memoryProblem(ram int64) (headline, detail string) {
	if ram == 0 || ram >= MinRAM {
		return "", ""
	}
	return "This Mac does not have the memory hum needs.",
		"Hum runs one model, " + Catalog[0].Name + ", and it asks for 32 GB of " +
			"unified memory. This Mac has " + humanBytes(ram) + ". The weights " +
			"alone are " + humanBytes(Catalog[0].Bytes) + " and the cache grows " +
			"on top of them, so there is nothing to trim. Smaller models do run " +
			"well on a Mac this size, and hum will carry one once it has been " +
			"measured rather than guessed at. Until then, point hum at your own " +
			"with `hum start --model /path/to/mlx/model`, or let LM Studio or " +
			"Ollama pick from their catalogues."
}

// checkMachine stops before anything expensive happens on a machine that cannot
// run the model at all. Naming your own model skips the memory check: whoever
// did that knows what fits.
func checkMachine(u *UI, custom bool) error {
	headline, detail := archProblem(runtime.GOOS, runtime.GOARCH, underRosetta())
	if headline == "" && !custom {
		headline, detail = memoryProblem(systemRAM())
	}
	if headline == "" {
		return nil
	}
	u.Fail("%s", headline)
	u.Para("%s", detail)
	return errQuiet
}
