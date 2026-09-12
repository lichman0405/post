package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/version"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(version) exit code = %d, want 0", code)
	}
	want := "rddev " + version.Version + "\n"
	if stdout.String() != want {
		t.Errorf("run(version) output = %q, want %q", stdout.String(), want)
	}
	if stderr.Len() != 0 {
		t.Errorf("run(version) stderr = %q, want empty", stderr.String())
	}
}

func TestRunUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"frobnicate"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run(unknown) exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown subcommand "frobnicate"`) {
		t.Errorf("run(unknown) stderr = %q, want unknown-subcommand message", stderr.String())
	}
}

func TestRunNoArgsShowsUsageAndExitsNonZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 {
		t.Fatalf("run(no args) exit code = %d, want 2", code)
	}
	if !strings.Contains(stdout.String(), "rddev version") {
		t.Errorf("run(no args) stdout = %q, want usage text", stdout.String())
	}
}
