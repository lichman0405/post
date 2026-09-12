package devorchestrator

import (
	"os"
	"path/filepath"
	"strings"
)

// The spawn environment contract (ported from the dispatch harness,
// L1-20260912-3): every remote credential the Supervisor's environment may
// carry is stripped before the Worker inherits it, and the guard contract
// variables are set. The guard's fail-closed checks (contract env missing =>
// refuse to decide) make a spawn that skips this function fail loudly instead
// of silently widening the Worker's reach.

// strippedEnvVars are removed whole from the Worker environment: a Worker
// holding a token could push even with the guard active (the guard blocks the
// command, the token removal makes the capability itself unavailable).
var strippedEnvVars = []string{
	"GH_TOKEN",
	"GITHUB_TOKEN",
	"GITHUB_PERSONAL_ACCESS_TOKEN",
	"GITHUB_ENTERPRISE_TOKEN",
	"SSH_AUTH_SOCK",
	"GITEA_TOKEN",
	"POST_GITEA_TOKEN",
}

// strippedEnvPrefixes are removed by prefix: AWS_* carries cloud credentials,
// DOCKER_* can carry registry/socket auth (DOCKER_HOST is restored only via
// the per-task Docker grant).
var strippedEnvPrefixes = []string{
	"AWS_",
	"DOCKER_",
}

// WorkerEnv is the resolved spawn environment for one task.
type WorkerEnv struct {
	// Dir is the absolute per-task runtime dir (.rddev/workers/<TASK>).
	Dir string
	// GHConfigDir is the neutralised GH_CONFIG_DIR (an empty dir so `gh` can
	// find no credentials even if a token leaks in some other way).
	GHConfigDir string
	// Vars is the full environment for the Worker process (all inherited
	// variables minus stripped credentials, plus the POST_* contract).
	Vars []string
}

// BuildWorkerEnv derives the Worker environment from the Supervisor's. The
// neutralised GH_CONFIG_DIR is created under the task dir, so it is cleared
// with the task's runtime state and never pollutes the Supervisor's config.
func BuildWorkerEnv(supervisorEnv []string, repoRoot, taskID, worktree, resultDir string, dockerGrant bool) (*WorkerEnv, error) {
	ghDir := filepath.Join(WorkerTaskDir(repoRoot, taskID), "gh-config")
	if err := os.MkdirAll(ghDir, 0o755); err != nil {
		return nil, err
	}

	post := map[string]string{
		"POST_REPO_ROOT":          repoRoot,
		"POST_WORKER_TASK_ID":     taskID,
		"POST_WORKER_WORKTREE":    worktree,
		"POST_WORKER_RESULT_DIR":  resultDir,
		"POST_WORKER_WORKERS_DIR": WorkersDir(repoRoot),
	}
	if dockerGrant {
		post["POST_WORKER_DOCKER_GRANT"] = "1"
		// COMPOSE_PROJECT_NAME namespaces the task's compose stack so two
		// Docker-granted tasks never share volumes/networks (harness parity).
		post["COMPOSE_PROJECT_NAME"] = "post-" + strings.ToLower(taskID)
	}

	seen := map[string]bool{}
	vars := make([]string, 0, len(supervisorEnv)+len(post))
	for _, kv := range supervisorEnv {
		key, _, _ := strings.Cut(kv, "=")
		if key == "GH_CONFIG_DIR" {
			continue // replaced by the neutralised dir below
		}
		if stripped(key) {
			continue
		}
		if _, ok := post[key]; ok {
			// contract vars are always set by spawn, never inherited
			continue
		}
		vars = append(vars, kv)
		seen[key] = true
	}
	vars = append(vars, "GH_CONFIG_DIR="+ghDir)
	for k, v := range post {
		vars = append(vars, k+"="+v)
	}
	return &WorkerEnv{Dir: WorkerTaskDir(repoRoot, taskID), GHConfigDir: ghDir, Vars: vars}, nil
}

func stripped(key string) bool {
	for _, k := range strippedEnvVars {
		if key == k {
			return true
		}
	}
	for _, p := range strippedEnvPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}
