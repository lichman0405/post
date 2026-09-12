package doctor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// measurements holds every measured input, before and after fixture
// overrides. Mirrors the variables of ops/doctor.sh.
type measurements struct {
	kernel, arch                                            string
	distroID, distroVersionID, distroPretty, codename       string
	wslKind                                                 string // none | wsl1 | wsl2
	repoPath, repoFSMount                                   string
	nproc, memKB, swapKB, fdLimit, diskKB                   string
	dockerReachable                                         bool
	dockerRoot                                              string
	dockerDiskKB                                            string
}

// fixtureVal returns the first line of the fixture file ("" if absent),
// mirroring fixture_val() in ops/doctor.sh (head -n1 | tr -d '\r').
func (o *Options) fixtureVal(name string) string {
	if o.FixtureDir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(o.FixtureDir, name))
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimSuffix(line, "\r")
}

// fixtureLines returns the first n lines of the fixture file ("" if absent),
// mirroring the tool-fixture reads (head -n5 | tr -d '\r').
func (o *Options) fixtureLines(name string, n int) string {
	if o.FixtureDir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(o.FixtureDir, name))
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.TrimSuffix(strings.Join(lines, "\n"), "\r")
}

// measure fills m from the real host (ops/doctor.sh measure_env +
// measure_resources + measure_docker_daemon), then applies fixture overrides
// per contract §6: a present fixture file overrides, otherwise real
// measurement stands. In fixture mode real version commands are never run.
func (o *Options) measure() *measurements {
	m := &measurements{}

	// --- environment -------------------------------------------------
	m.kernel = unameField("-s")
	m.arch = unameField("-m")
	if os.Getenv("OS") == "Windows_NT" { // contract §7.1: $OS == Windows_NT
		m.kernel = "Windows_NT"
	}
	for k, v := range osReleaseFields() {
		switch k {
		case "ID":
			m.distroID = v
		case "VERSION_ID":
			m.distroVersionID = v
		case "PRETTY_NAME":
			m.distroPretty = v
		case "VERSION_CODENAME":
			m.codename = v
		}
	}
	m.wslKind = "none"
	if version, err := os.ReadFile("/proc/version"); err == nil {
		if strings.Contains(strings.ToLower(string(version)), "microsoft") {
			if strings.Contains(strings.ToLower(string(version)), "wsl2") {
				m.wslKind = "wsl2"
			} else {
				m.wslKind = "wsl1"
			}
		}
	}
	if wd, err := os.Getwd(); err == nil {
		if resolved, err := filepath.EvalSymlinks(wd); err == nil {
			wd = resolved
		}
		m.repoPath = wd
	} else if wd, err := os.Getwd(); err == nil {
		m.repoPath = wd
	}
	if m.repoPath != "" {
		m.repoFSMount = dfColumn(m.repoPath, 6)
	}
	if v := o.fixtureVal("os.kernel"); v != "" {
		m.kernel = v
	}
	if v := o.fixtureVal("os.arch"); v != "" {
		m.arch = v
	}
	if v := o.fixtureVal("os.id"); v != "" {
		m.distroID = v
	}
	if v := o.fixtureVal("os.version_id"); v != "" {
		m.distroVersionID = v
	}
	if v := o.fixtureVal("os.pretty"); v != "" {
		m.distroPretty = v
	}
	if v := o.fixtureVal("os.codename"); v != "" {
		m.codename = v
	}
	if v := o.fixtureVal("os.wsl"); v != "" {
		m.wslKind = v
	}
	if v := o.fixtureVal("os.repo_path"); v != "" {
		m.repoPath = v
	}

	// --- resources ----------------------------------------------------
	m.nproc = execOutput("nproc")
	m.memKB = meminfoKB("MemTotal")
	m.swapKB = meminfoKB("SwapTotal")
	if lim, err := fdSoftLimit(); err == nil {
		m.fdLimit = strconv.FormatUint(lim, 10)
	}
	if m.repoPath != "" {
		m.diskKB = dfColumn(m.repoPath, 4)
	}
	if v := o.fixtureVal("res.nproc"); v != "" {
		m.nproc = v
	}
	if v := o.fixtureVal("res.mem_total_kb"); v != "" {
		m.memKB = v
	}
	if v := o.fixtureVal("res.swap_total_kb"); v != "" {
		m.swapKB = v
	}
	if v := o.fixtureVal("res.fd_limit"); v != "" {
		m.fdLimit = v
	}
	if v := o.fixtureVal("res.disk_free_kb"); v != "" {
		m.diskKB = v
	}

	// --- docker daemon (only probed when --check-docker-daemon) -------
	if o.CheckDockerDaemon {
		if v := o.fixtureVal("res.docker_reachable"); v != "" {
			m.dockerReachable = v == "true"
		} else {
			m.dockerReachable = execOK(10*time.Second, "docker", "info")
		}
		if v := o.fixtureVal("res.docker_root_free_kb"); v != "" {
			m.dockerDiskKB = v
		} else if m.dockerReachable {
			if root := execOutput("docker", "info", "--format", "{{.DockerRootDir}}"); root != "" {
				m.dockerRoot = root
				m.dockerDiskKB = dfColumn(root, 4)
			}
		}
	}
	return m
}

// unameField runs `uname <flag>` and returns its output ("" on failure).
func unameField(flag string) string {
	out, err := exec.Command("uname", flag).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// osReleaseFields parses KEY=VALUE pairs from /etc/os-release.
func osReleaseFields() map[string]string {
	fields := map[string]string{}
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return fields
	}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		fields[k] = strings.Trim(v, `"`)
	}
	return fields
}

// meminfoKB reads a kB field from /proc/meminfo ("" on failure).
func meminfoKB(field string) string {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		rest, ok := strings.CutPrefix(line, field+":")
		if !ok {
			continue
		}
		return strings.TrimSpace(strings.Fields(rest)[0])
	}
	return ""
}

// fdSoftLimit returns the soft RLIMIT_NOFILE (bash `ulimit -n`).
func fdSoftLimit() (uint64, error) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0, err
	}
	return lim.Cur, nil
}

// dfColumn returns column col (1-based) of the second `df -Pk <path>` line.
func dfColumn(path string, col int) string {
	out, err := exec.Command("df", "-Pk", path).Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) < 2 {
		return ""
	}
	fields := strings.Fields(lines[1])
	if len(fields) < col {
		return ""
	}
	return fields[col-1]
}

// execOutput runs cmd and returns trimmed stdout ("" on failure).
func execOutput(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// execOK reports whether cmd succeeds within the timeout (output discarded).
func execOK(timeout time.Duration, name string, args ...string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Run() == nil
}

// toolMeasurement probes one tool the way ops/doctor.sh does:
// `command -v <bin>` for presence, then `timeout 15 bash -c '<vercmd>'` with
// the first 5 lines of combined output kept. Never run in fixture mode when
// the fixture provides the tool.
type toolMeasurement struct {
	present bool
	raw     string // first 5 lines of version output
}

// measureTool probes tool id (real execution). Not used in fixture mode when
// the fixture decides the tool.
func measureTool(def toolDef) toolMeasurement {
	tm := toolMeasurement{}
	if _, err := exec.LookPath(def.bin); err != nil {
		return tm
	}
	tm.present = true
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", def.verCmd)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		// The command failing after the binary exists still yields output;
		// ops/doctor.sh keeps whatever was printed.
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) > 5 {
		lines = lines[:5]
	}
	tm.raw = strings.TrimSuffix(strings.Join(lines, "\n"), "\r")
	return tm
}

// toolInput resolves the tool measurement source for id: fixture file,
// fixture .absent marker, or a real probe (contract §6: fixture files exist
// to override; absence falls back to real measurement).
func (o *Options) toolInput(id string) toolMeasurement {
	def := toolDefs[id]
	if o.FixtureDir != "" {
		if raw := o.fixtureLines(def.fix, 5); raw != "" {
			return toolMeasurement{present: true, raw: raw}
		}
		if _, err := os.Stat(filepath.Join(o.FixtureDir, def.fix+".absent")); err == nil {
			return toolMeasurement{present: false}
		}
	}
	return measureTool(def)
}
