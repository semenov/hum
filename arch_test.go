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
