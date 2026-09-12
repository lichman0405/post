package main

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
)

// recordCompose replaces composeExec for the duration of the test: nothing
// Docker is ever executed in unit tests (the real compose invocation is a G3
// integration check against the local stack).
func recordCompose(t *testing.T, fail bool) *[][]string {
	t.Helper()
	var calls [][]string
	orig := composeExec
	composeExec = func(stdout, stderr io.Writer, args ...string) int {
		call := append([]string(nil), args...)
		calls = append(calls, call)
		if fail {
			return exitOperational
		}
		return exitOK
	}
	t.Cleanup(func() { composeExec = orig })
	return &calls
}

func TestEnvUpWrapsCompose(t *testing.T) {
	calls := recordCompose(t, false)
	var stdout, stderr bytes.Buffer
	if code := runEnv([]string{"up"}, &stdout, &stderr, false); code != 0 {
		t.Fatalf("env up exit code = %d, want 0", code)
	}
	want := []string{"-f", "docker-compose.yml", "up", "-d", "--wait", "--wait-timeout", "300"}
	if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0], want) {
		t.Fatalf("compose args = %v, want %v", *calls, want)
	}
}

func TestEnvDownWrapsCompose(t *testing.T) {
	calls := recordCompose(t, false)
	var stdout, stderr bytes.Buffer
	if code := runEnv([]string{"down"}, &stdout, &stderr, false); code != 0 {
		t.Fatalf("env down exit code = %d, want 0", code)
	}
	want := []string{"-f", "docker-compose.yml", "down"}
	if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0], want) {
		t.Fatalf("compose args = %v, want %v", *calls, want)
	}
}

func TestEnvUpComposeFailureIsExitOne(t *testing.T) {
	recordCompose(t, true)
	var stdout, stderr bytes.Buffer
	if code := runEnv([]string{"up"}, &stdout, &stderr, false); code != 1 {
		t.Fatalf("env up (compose failing) exit code = %d, want 1", code)
	}
}

func TestEnvResetAndGCAreHonestStubs(t *testing.T) {
	// No composeExec call must happen for reset/gc.
	calls := recordCompose(t, false)
	var stdout, stderr bytes.Buffer
	if code := runEnv([]string{"reset", "--test-only"}, &stdout, &stderr, false); code != 4 {
		t.Fatalf("env reset exit code = %d, want 4", code)
	}
	if len(*calls) != 0 {
		t.Fatalf("env reset executed docker compose: %v", *calls)
	}
	msg := stderr.String()
	if !strings.Contains(msg, "not implemented") || !strings.Contains(msg, "T0010/T0011") {
		t.Errorf("env reset message = %q, want honest stub naming T0010/T0011", msg)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runEnv([]string{"gc"}, &stdout, &stderr, false); code != 4 {
		t.Fatalf("env gc exit code = %d, want 4", code)
	}
	if !strings.Contains(stderr.String(), "not implemented") {
		t.Errorf("env gc message = %q", stderr.String())
	}

	// JSON mode: a machine-readable error object on stdout.
	stdout.Reset()
	stderr.Reset()
	if code := runEnv([]string{"reset", "--json"}, &stdout, &stderr, true); code != 4 {
		t.Fatalf("env reset --json exit code = %d, want 4", code)
	}
	var doc map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("env reset --json stdout invalid: %v\n%s", err, stdout.String())
	}
	if doc["exit_code"] != float64(4) {
		t.Errorf("env reset --json exit_code = %v, want 4", doc["exit_code"])
	}
}

func TestEnvUsageErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runEnv(nil, &stdout, &stderr, false); code != 2 {
		t.Fatalf("env (no subcommand) exit code = %d, want 2", code)
	}
	if code := runEnv([]string{"bogus"}, &stdout, &stderr, false); code != 2 {
		t.Fatalf("env bogus exit code = %d, want 2", code)
	}
	if code := runEnv([]string{"up", "extra"}, &stdout, &stderr, false); code != 2 {
		t.Fatalf("env up extra exit code = %d, want 2", code)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runEnv([]string{"--help"}, &stdout, &stderr, false); code != 0 {
		t.Fatalf("env --help exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "docker compose up") {
		t.Errorf("env --help stdout = %q, want env usage", stdout.String())
	}
}
