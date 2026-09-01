package main

import "testing"

func TestArchProblem(t *testing.T) {
	cases := []struct {
		name, goos, goarch string
		rosetta            bool
		wantProblem        bool
		wantIn             string
	}{
		{"apple silicon", "darwin", "arm64", false, false, ""},
		{"intel mac", "darwin", "amd64", false, true, "Intel chip"},
		{"rosetta", "darwin", "amd64", true, true, "Rosetta"},
		{"linux", "linux", "amd64", false, true, "only runs on a Mac"},
		// Rosetta only matters on a Mac; elsewhere the OS is the problem.
		{"linux, flag set", "linux", "arm64", true, true, "only runs on a Mac"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			head, detail := archProblem(c.goos, c.goarch, c.rosetta)
			if (head != "") != c.wantProblem {
				t.Fatalf("headline = %q, want problem = %v", head, c.wantProblem)
			}
			if !c.wantProblem {
				return
			}
			if detail == "" {
				t.Error("a problem with no explanation is not much use")
			}
			if !contains(head+detail, c.wantIn) {
				t.Errorf("message %q + %q does not mention %q", head, detail, c.wantIn)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestMemoryProblem(t *testing.T) {
	cases := []struct {
		name        string
		ram         int64
		wantProblem bool
	}{
		{"plenty", 64 * gb, false},
		{"exactly the minimum", MinRAM, false},
		{"one byte short", MinRAM - 1, true},
		{"24 GB", 24 * gb, true},
		{"8 GB", 8 * gb, true},
		// sysctl failed; refusing to start over a number we could not read
		// would be worse than letting the machine try.
		{"unreadable", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			head, detail := memoryProblem(c.ram)
			if (head != "") != c.wantProblem {
				t.Fatalf("headline = %q, want problem = %v", head, c.wantProblem)
			}
			if !c.wantProblem {
				return
			}
			// The message has to name this Mac's own size, or it reads like a
			// generic wall rather than an answer about the machine in front of you.
			if !contains(detail, humanBytes(c.ram)) {
				t.Errorf("detail %q does not mention the machine's %s", detail, humanBytes(c.ram))
			}
			if !contains(detail, "--model") {
				t.Error("a refusal with no way forward is just a wall")
			}
		})
	}
}
