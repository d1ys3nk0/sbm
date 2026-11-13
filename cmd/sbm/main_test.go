package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHelpVersionAndUsageFromEntrypoint(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "sbm")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	for _, tc := range []struct {
		args     []string
		exit     int
		contains string
	}{
		{[]string{"--help"}, 0, "Usage: sbm"},
		{[]string{"--version"}, 0, "sbm dev"},
		{[]string{"not-a-command"}, 2, "invalid command or arguments"},
		{[]string{"status", "unexpected"}, 2, "invalid command or arguments"},
	} {
		out, err := exec.Command(binary, tc.args...).CombinedOutput()
		code := 0
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if code != tc.exit || !strings.Contains(string(out), tc.contains) {
			t.Errorf("%v: code %d, output %q", tc.args, code, out)
		}
	}
}
