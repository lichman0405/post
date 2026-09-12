package devorchestrator

import (
	"os"
	"strings"
	"testing"
)

// TestBuildWorkerEnvStripsCredentials: every remote credential and AWS/Docker
// variable is removed, GH_CONFIG_DIR is neutralised to an empty dir, and the
// guard contract variables are set (L1-20260912-3 / F-20260912-2).
func TestBuildWorkerEnvStripsCredentials(t *testing.T) {
	repo := t.TempDir()
	sup := []string{
		"PATH=/usr/bin",
		"GH_TOKEN=ghp_secret",
		"GITHUB_TOKEN=ghs_secret",
		"GITHUB_PERSONAL_ACCESS_TOKEN=pat_secret",
		"GITHUB_ENTERPRISE_TOKEN=ent_secret",
		"SSH_AUTH_SOCK=/run/ssh-agent.sock",
		"GITEA_TOKEN=gitea_secret",
		"POST_GITEA_TOKEN=post_gitea_secret",
		"AWS_ACCESS_KEY_ID=AKIAx",
		"AWS_SECRET_ACCESS_KEY=aws_secret",
		"AWS_SESSION_TOKEN=aws_session",
		"DOCKER_HOST=tcp://evil:2375",
		"DOCKER_AUTH_CONFIG={\"auths\":{}}",
		"GH_CONFIG_DIR=/home/x/.config/gh",
		"POST_REPO_ROOT=/should/be/overridden",
		"HOME=/home/x",
		"ANTHROPIC_BASE_URL=http://gateway",
	}
	env, err := BuildWorkerEnv(sup, repo, "T0001", "/repo/.rddev/worktrees/T0001", "/repo/.rddev/workers/T0001", false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, kv := range env.Vars {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GITHUB_PERSONAL_ACCESS_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "SSH_AUTH_SOCK", "GITEA_TOKEN", "POST_GITEA_TOKEN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "DOCKER_HOST", "DOCKER_AUTH_CONFIG"} {
		if _, present := got[k]; present {
			t.Errorf("credential %s leaked into the Worker environment", k)
		}
	}
	// inherited non-credentials survive (the Worker needs PATH and the gateway)
	if got["PATH"] != "/usr/bin" || got["ANTHROPIC_BASE_URL"] != "http://gateway" {
		t.Errorf("non-credential environment lost: %v", got)
	}
	// GH_CONFIG_DIR is neutralised, not inherited
	if got["GH_CONFIG_DIR"] != env.GHConfigDir {
		t.Errorf("GH_CONFIG_DIR = %q, want the neutralised dir %q", got["GH_CONFIG_DIR"], env.GHConfigDir)
	}
	entries, err := os.ReadDir(env.GHConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("neutralised GH_CONFIG_DIR is not empty: %d entries", len(entries))
	}
	// contract vars set by spawn, never inherited
	if got["POST_REPO_ROOT"] != repo || got["POST_WORKER_TASK_ID"] != "T0001" || got["POST_WORKER_WORKTREE"] != "/repo/.rddev/worktrees/T0001" || got["POST_WORKER_RESULT_DIR"] != "/repo/.rddev/workers/T0001" || got["POST_WORKER_WORKERS_DIR"] != WorkersDir(repo) {
		t.Errorf("contract vars wrong: %v", got)
	}
	// no docker grant: no DOCKER_GRANT, no compose namespacing
	if _, present := got["POST_WORKER_DOCKER_GRANT"]; present {
		t.Error("POST_WORKER_DOCKER_GRANT set without a grant")
	}
	if _, present := got["COMPOSE_PROJECT_NAME"]; present {
		t.Error("COMPOSE_PROJECT_NAME set without a grant")
	}
}

// TestBuildWorkerEnvDockerGrant: --docker sets the guard grant and the
// compose project namespacing so granted tasks never share stacks.
func TestBuildWorkerEnvDockerGrant(t *testing.T) {
	repo := t.TempDir()
	env, err := BuildWorkerEnv(nil, repo, "T0001", "/w", "/r", true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, kv := range env.Vars {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if got["POST_WORKER_DOCKER_GRANT"] != "1" {
		t.Error("POST_WORKER_DOCKER_GRANT != 1 with the grant")
	}
	if got["COMPOSE_PROJECT_NAME"] != "post-t0001" {
		t.Errorf("COMPOSE_PROJECT_NAME = %q, want post-t0001", got["COMPOSE_PROJECT_NAME"])
	}
}

// TestStrippedTable: the exact names, so a rename never silently un-strips.
func TestStrippedTable(t *testing.T) {
	if !stripped("GH_TOKEN") || !stripped("SSH_AUTH_SOCK") || !stripped("AWS_REGION") || !stripped("DOCKER_CONFIG") {
		t.Error("stripped() missed a credential name")
	}
	if stripped("HOME") || stripped("PATH") || stripped("ANTHROPIC_BASE_URL") || stripped("GITHUB_USER") {
		t.Error("stripped() over-stripped a benign variable")
	}
}
