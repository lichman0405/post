package doctor

import (
	"encoding/json"
	"fmt"
	"io"
)

// EmitHuman prints the human-readable report, byte-compatible with
// ops/doctor.sh except the script name in the header line.
func EmitHuman(w io.Writer, scriptName string, m *measurements, checks []Check, verdict string, exitCode int) {
	fmt.Fprintf(w, "POST environment preflight (%s) — contract %s\n", scriptName, "ops/doctor-checks.md")
	kernel, arch := m.kernel, m.arch
	if kernel == "" {
		kernel = "unknown"
	}
	if arch == "" {
		arch = "unknown"
	}
	distroID, distroVer, codename := m.distroID, m.distroVersionID, m.codename
	if distroID == "" {
		distroID = "unknown"
	}
	if distroVer == "" {
		distroVer = "unknown"
	}
	if codename == "" {
		codename = "unknown"
	}
	fmt.Fprintf(w, "Environment: kernel=%s arch=%s distro=%s %s (%s) wsl=%s\n",
		kernel, arch, distroID, distroVer, codename, m.wslKind)
	mount := m.repoFSMount
	if mount == "" {
		mount = "unknown"
	}
	fmt.Fprintf(w, "Repo: %s (fs mount: %s)\n\n", m.repoPath, mount)

	for _, c := range checks {
		tag := map[string]string{
			"passed": "PASS", "failed": "FAIL", "warn": "WARN", "skipped": "SKIP",
		}[c.Status]
		fmt.Fprintf(w, "[%s] %-17s %s — baseline: %s\n", tag, c.ID, c.Measured, c.Baseline)
		switch c.Status {
		case "failed", "warn":
			fmt.Fprintf(w, "       fix: %s\n", c.Remediation)
		case "skipped":
			fmt.Fprintf(w, "       note: %s\n", c.Reason)
		}
	}

	rt, rp, rf, rs, at, ap, aw, as := 0, 0, 0, 0, 0, 0, 0, 0
	for _, c := range checks {
		if c.Severity == "required" {
			rt++
			switch c.Status {
			case "passed":
				rp++
			case "failed":
				rf++
			case "skipped":
				rs++
			}
		} else {
			at++
			switch c.Status {
			case "passed":
				ap++
			case "warn":
				aw++
			case "skipped":
				as++
			}
		}
	}
	fmt.Fprintf(w, "\nSummary: required %d checks: %d passed, %d failed, %d skipped | advisory %d checks: %d passed, %d warn, %d skipped\n",
		rt, rp, rf, rs, at, ap, aw, as)
	fmt.Fprintf(w, "Verdict: %s (exit %d)\n", verdict, exitCode)
}

// JSON field order is part of the contract (ops/doctor-output.schema.json and
// the reference implementation): struct field order = emission order.
type environmentJSON struct {
	Kernel           string `json:"kernel"`
	Arch             string `json:"arch"`
	DistroID         string `json:"distro_id"`
	DistroVersionID  string `json:"distro_version_id"`
	DistroPretty     string `json:"distro_pretty"`
	Codename         string `json:"codename"`
	WSL              string `json:"wsl"`
	RepoPath         string `json:"repo_path"`
	RepoFSMount      string `json:"repo_fs_mount"`
}

type baselinesJSON struct {
	Go           string `json:"go"`
	Node         string `json:"node"`
	Python3      string `json:"python3"`
	Psql         string `json:"psql"`
	RedisCLI     string `json:"redis-cli"`
	Git          string `json:"git"`
	GitLFS       string `json:"git-lfs"`
	Jq           string `json:"jq"`
	Make         string `json:"make"`
	Shellcheck   string `json:"shellcheck"`
	DockerCompose string `json:"docker-compose"`
	Claude       string `json:"claude"`
	UV           string `json:"uv"`
	Pnpm         string `json:"pnpm"`
	CPU          string `json:"cpu"`
	RAM          string `json:"ram"`
	Swap         string `json:"swap"`
	Disk         string `json:"disk"`
	FD           string `json:"fd"`
	DockerDisk   string `json:"docker-disk"`
}

type checkJSON struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Category    string  `json:"category"`
	Severity    string  `json:"severity"`
	Status      string  `json:"status"`
	Measured    string  `json:"measured"`
	Version     *string `json:"version"`
	Baseline    string  `json:"baseline"`
	Remediation *string `json:"remediation"`
	Reason      *string `json:"reason"`
}

type summaryJSON struct {
	Required requiredSummary `json:"required"`
	Advisory advisorySummary `json:"advisory"`
}

type requiredSummary struct {
	Total   int `json:"total"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

type advisorySummary struct {
	Total   int `json:"total"`
	Passed  int `json:"passed"`
	Warn    int `json:"warn"`
	Skipped int `json:"skipped"`
}

type doctorOutput struct {
	SchemaVersion int             `json:"schema_version"`
	Contract      string          `json:"contract"`
	Verdict       string          `json:"verdict"`
	ExitCode      int             `json:"exit_code"`
	Environment   environmentJSON `json:"environment"`
	Baselines     baselinesJSON   `json:"baselines"`
	Checks        []checkJSON     `json:"checks"`
	Summary       summaryJSON     `json:"summary"`
}

// EmitJSON prints the machine-readable report (exact contract shape).
func EmitJSON(w io.Writer, m *measurements, checks []Check, verdict string, exitCode int) {
	out := doctorOutput{
		SchemaVersion: 1,
		Contract:      "ops/doctor-checks.md",
		Verdict:       verdict,
		ExitCode:      exitCode,
		Environment: environmentJSON{
			Kernel: m.kernel, Arch: m.arch, DistroID: m.distroID,
			DistroVersionID: m.distroVersionID, DistroPretty: m.distroPretty,
			Codename: m.codename, WSL: m.wslKind,
			RepoPath: m.repoPath, RepoFSMount: m.repoFSMount,
		},
		Baselines: baselinesJSON{
			Go: "1.27.x", Node: "24.x", Python3: ">= 3.12", Psql: "16.x",
			RedisCLI: "7.x", Git: "2.x", GitLFS: "3.x", Jq: "1.x", Make: "4.x",
			Shellcheck: "recorded", DockerCompose: "v2 plugin lineage (>= 2)",
			Claude: "recorded", UV: "recorded", Pnpm: "packageManager pin",
			CPU: ">= 8", RAM: ">= 32 GiB", Swap: ">= 8 GiB", Disk: ">= 20 GiB",
			FD: ">= 65535", DockerDisk: ">= 100 GiB",
		},
	}
	for _, c := range checks {
		cj := checkJSON{
			ID: c.ID, Name: c.Name, Category: c.Category, Severity: c.Severity,
			Status: c.Status, Measured: c.Measured, Baseline: c.Baseline,
		}
		if c.Version != "" {
			v := c.Version
			cj.Version = &v
		}
		if c.Remediation != "" {
			r := c.Remediation
			cj.Remediation = &r
		}
		if c.Reason != "" {
			r := c.Reason
			cj.Reason = &r
		}
		out.Checks = append(out.Checks, cj)
	}
	for _, c := range checks {
		if c.Severity == "required" {
			out.Summary.Required.Total++
			switch c.Status {
			case "passed":
				out.Summary.Required.Passed++
			case "failed":
				out.Summary.Required.Failed++
			case "skipped":
				out.Summary.Required.Skipped++
			}
		} else {
			out.Summary.Advisory.Total++
			switch c.Status {
			case "passed":
				out.Summary.Advisory.Passed++
			case "warn":
				out.Summary.Advisory.Warn++
			case "skipped":
				out.Summary.Advisory.Skipped++
			}
		}
	}
	// Compact output, raw unicode (no HTML escaping): byte-identical to the
	// reference implementation's hand-built JSON for the same inputs.
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(out)
}
