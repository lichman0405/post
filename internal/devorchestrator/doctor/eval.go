package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Check is one evaluated preflight check (contract §7).
type Check struct {
	ID          string
	Name        string
	Category    string // env | resource | tool
	Severity    string // required | advisory
	Status      string // passed | failed | warn | skipped
	Measured    string
	Version     string // first X.Y pair, "" when not applicable
	Baseline    string
	Remediation string // non-empty exactly when failed or warn
	Reason      string // non-empty exactly when skipped
}

// statusOK reports whether a check status is in the given set.
func (c Check) ok() bool { return c.Status == "passed" || c.Status == "skipped" }

// evaluate runs all 35 checks in canonical order against the measurements.
func evaluate(o *Options, m *measurements) ([]Check, string, int) {
	var checks []Check
	checks = append(checks, evalEnv(m)...)
	checks = append(checks, evalResources(o, m)...)
	checks = append(checks, evalTools(o, m)...)

	reqFailed, envFailed := false, false
	for _, c := range checks {
		if c.Severity == "required" && c.Status == "failed" {
			reqFailed = true
			if c.Category == "env" {
				envFailed = true
			}
		}
	}
	switch {
	case envFailed:
		return checks, "unsupported", 2
	case reqFailed:
		return checks, "toolchain_failure", 1
	default:
		return checks, "ok", 0
	}
}

func setCheck(c *Check, id, name, category, severity, status, measured, version, baseline, remediation, reason string) {
	c.ID, c.Name, c.Category, c.Severity = id, name, category, severity
	c.Status, c.Measured, c.Version, c.Baseline = status, measured, version, baseline
	c.Remediation, c.Reason = remediation, reason
}

// ---- ENV (contract §7.1) ------------------------------------------------

func evalEnv(m *measurements) []Check {
	var out []Check

	var c Check
	kern := m.kernel
	if os.Getenv("OS") == "Windows_NT" {
		kern = "Windows_NT"
	}
	switch {
	case kern == "Linux":
		setCheck(&c, "ENV-OS-KERNEL", "OS kernel", "env", "required", "passed", "kernel: "+m.kernel, "", "Linux", "", "")
	case strings.HasPrefix(kern, "MINGW") || strings.HasPrefix(kern, "MSYS") ||
		strings.HasPrefix(kern, "CYGWIN") || kern == "Windows_NT":
		setCheck(&c, "ENV-OS-KERNEL", "OS kernel", "env", "required", "failed", "kernel: "+kern+" (Windows Native)", "", "Linux", remWindowsNative, "")
	default:
		setCheck(&c, "ENV-OS-KERNEL", "OS kernel", "env", "required", "failed", "kernel: "+kern, "", "Linux", remOtherKernel, "")
	}
	out = append(out, c)

	c = Check{}
	if m.distroID == "ubuntu" && m.distroVersionID == "24.04" {
		pretty := m.distroPretty
		if pretty == "" {
			pretty = "ubuntu"
		}
		setCheck(&c, "ENV-OS-DISTRO", "OS distro", "env", "required", "passed",
			"distro: "+pretty+" "+m.distroVersionID+" ("+m.codename+")", "", "ubuntu 24.04 (noble)", "", "")
	} else {
		id, ver := m.distroID, m.distroVersionID
		if id == "" {
			id = "unknown"
		}
		if ver == "" {
			ver = "unknown"
		}
		setCheck(&c, "ENV-OS-DISTRO", "OS distro", "env", "required", "failed",
			"distro: "+id+" "+ver, "", "ubuntu 24.04 (noble)", remDistro, "")
	}
	out = append(out, c)

	c = Check{}
	if m.arch == "x86_64" {
		setCheck(&c, "ENV-OS-ARCH", "CPU architecture", "env", "required", "passed", "arch: "+m.arch, "", "x86_64 (amd64)", "", "")
	} else {
		arch := m.arch
		if arch == "" {
			arch = "unknown"
		}
		setCheck(&c, "ENV-OS-ARCH", "CPU architecture", "env", "required", "failed", "arch: "+arch, "", "x86_64 (amd64)", remArch, "")
	}
	out = append(out, c)

	c = Check{}
	if m.wslKind == "wsl1" {
		setCheck(&c, "ENV-WSL", "WSL kind", "env", "advisory", "warn", "WSL: "+m.wslKind, "", "wsl2 or none", remWSL1, "")
	} else {
		setCheck(&c, "ENV-WSL", "WSL kind", "env", "advisory", "passed", "WSL: "+m.wslKind, "", "wsl2 or none", "", "")
	}
	out = append(out, c)

	return out
}

// ---- resources (contract §7.2) --------------------------------------------

// evalRes implements the generic numeric-threshold check of doctor.sh.
func evalRes(id, src string, min int64, severity, rem, base string, m *measurements) Check {
	var c Check
	cur := ""
	switch src {
	case "nproc":
		cur = m.nproc
	case "mem_kb":
		cur = m.memKB
	case "swap_kb":
		cur = m.swapKB
	case "fd_limit":
		cur = m.fdLimit
	case "disk_kb":
		cur = m.diskKB
	}
	if cur == "" || !isNumeric(cur) {
		setCheck(&c, id, "system resource", "resource", severity, "failed", "unable to measure ("+src+")", "", base, remResUnmeasurable, "")
		return c
	}
	n, _ := strconv.ParseInt(cur, 10, 64)
	disp := ""
	switch src {
	case "nproc":
		disp = cur + " cores"
	case "mem_kb":
		disp = kbToGiB(cur) + " GiB total"
	case "swap_kb":
		disp = kbToGiB(cur) + " GiB total"
	case "fd_limit":
		disp = cur
	case "disk_kb":
		disp = kbToGiB(cur) + " GiB free"
	}
	switch {
	case n >= min:
		setCheck(&c, id, "system resource", "resource", severity, "passed", disp, "", base, "", "")
	case severity == "advisory":
		setCheck(&c, id, "system resource", "resource", severity, "warn", disp, "", base, rem, "")
	default:
		setCheck(&c, id, "system resource", "resource", severity, "failed", disp, "", base, rem, "")
	}
	return c
}

func kbToGiB(kb string) string {
	n, err := strconv.ParseInt(kb, 10, 64)
	if err != nil || n < 0 {
		n = 0
	}
	return strconv.FormatInt(n/giB, 10)
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func evalResources(o *Options, m *measurements) []Check {
	var out []Check
	out = append(out,
		evalRes("RES-CPU", "nproc", minCPU, "required", remCPU, ">= 8", m),
		evalRes("RES-CPU-TIER", "nproc", minCPUTier, "advisory", remCPUTier, ">= 16 (recommended)", m),
		evalRes("RES-RAM", "mem_kb", minRAMKB, "required", remRAM, ">= 32 GiB", m),
		evalRes("RES-RAM-TIER", "mem_kb", minRAMTierKB, "advisory", remRAMTier, ">= 64 GiB (recommended)", m),
		evalRes("RES-SWAP", "swap_kb", minSwapKB, "required", remSwap, ">= 8 GiB", m),
		evalRes("RES-SWAP-TIER", "swap_kb", minSwapTierKB, "advisory", remSwapTier, ">= 16 GiB (recommended)", m),
		evalRes("RES-DISK", "disk_kb", minDiskKB, "required", remDisk, ">= 20 GiB free", m),
		evalRes("RES-DISK-TIER", "disk_kb", minDiskTierKB, "advisory", remDiskTier, ">= 100 GiB free (recommended)", m),
		evalRes("RES-FD", "fd_limit", minFD, "required", remFD, ">= 65535", m),
	)

	// RES-DOCKER-DISK (required when measurable; needs --check-docker-daemon)
	base := ">= 100 GiB free on docker data-root"
	var c Check
	if !o.CheckDockerDaemon {
		setCheck(&c, "RES-DOCKER-DISK", "docker data-root free space", "resource", "required", "skipped",
			"disabled (opt-in)", "", base, "", reasonDockerOptIn)
	} else if !m.dockerReachable {
		setCheck(&c, "RES-DOCKER-DISK", "docker data-root free space", "resource", "required", "skipped",
			"daemon unreachable", "", base, "", reasonDockerDown)
	} else {
		kb := m.dockerDiskKB
		disp := "unable to measure (docker data-root)"
		if isNumeric(kb) {
			disp = kbToGiB(kb) + " GiB free"
		}
		switch {
		case isNumeric(kb) && atLeast(kb, minDockerDiskKB):
			setCheck(&c, "RES-DOCKER-DISK", "docker data-root free space", "resource", "required", "passed", disp, "", base, "", "")
		case isNumeric(kb):
			setCheck(&c, "RES-DOCKER-DISK", "docker data-root free space", "resource", "required", "failed", disp, "", base, remDockerDisk, "")
		default:
			setCheck(&c, "RES-DOCKER-DISK", "docker data-root free space", "resource", "required", "failed", disp, "", base, remDockerDiskNoM, "")
		}
	}
	out = append(out, c)

	// RES-REPO-FS (required only under WSL)
	c = Check{}
	if m.wslKind == "none" {
		setCheck(&c, "RES-REPO-FS", "repo filesystem (WSL)", "resource", "required", "skipped",
			"not running under WSL", "", "repo not under /mnt/", "", reasonRepoFS)
	} else if strings.HasPrefix(m.repoPath, "/mnt/") {
		setCheck(&c, "RES-REPO-FS", "repo filesystem (WSL)", "resource", "required", "failed",
			"repo path: "+m.repoPath, "", "repo not under /mnt/", remRepoFS, "")
	} else {
		setCheck(&c, "RES-REPO-FS", "repo filesystem (WSL)", "resource", "required", "passed",
			"repo path: "+m.repoPath, "", "repo not under /mnt/", "", "")
	}
	out = append(out, c)

	// RES-BWRAP / RES-SOCAT (advisory presence)
	out = append(out, evalPresenceRes(o, "RES-BWRAP", "bwrap", "bubblewrap", "bubblewrap", remBwrap))
	out = append(out, evalPresenceRes(o, "RES-SOCAT", "socat", "socat", "socat", remSocat))
	return out
}

func atLeast(kb string, min int64) bool {
	n, err := strconv.ParseInt(kb, 10, 64)
	return err == nil && n >= min
}

// evalPresenceRes implements the bwrap/socat presence checks: the fixture
// file decides when it says present/absent, otherwise the real binary is
// probed with command -v (contract §6 fallback rule).
func evalPresenceRes(o *Options, id, fix, bin, name, rem string) Check {
	var c Check
	present := false
	if o.FixtureDir != "" {
		switch o.fixtureVal("res." + fix) {
		case "present":
			present = true
		case "absent":
			present = false
		default:
			_, err := exec.LookPath(bin)
			present = err == nil
		}
	} else {
		_, err := exec.LookPath(bin)
		present = err == nil
	}
	if present {
		setCheck(&c, id, "sandbox prerequisite", "resource", "advisory", "passed", name+": present", "", "present", "", "")
	} else {
		setCheck(&c, id, "sandbox prerequisite", "resource", "advisory", "warn", name+": missing", "", "present", rem, "")
	}
	return c
}

// ---- tools (contract §7.3) ------------------------------------------------

var pairRe = regexp.MustCompile(`[0-9]+\.[0-9]+`)

// firstPair extracts the first X.Y numeric pair from the full output
// (grep -oE '[0-9]+\.[0-9]+' | head -n1 over the whole output).
func firstPair(s string) string { return pairRe.FindString(s) }

// cmpPairGe compares version pairs a >= b numerically (10# guard: decimal).
func cmpPairGe(a, b string) bool {
	aMaj, aMin, _ := strings.Cut(a, ".")
	bMaj, bMin, _ := strings.Cut(b, ".")
	am, _ := strconv.Atoi(aMaj)
	bm, _ := strconv.Atoi(bMaj)
	if am != bm {
		return am > bm
	}
	aMinor, bMinor := 0, 0
	if aMin != "" {
		aMinor, _ = strconv.Atoi(aMin)
	}
	if bMin != "" {
		bMinor, _ = strconv.Atoi(bMin)
	}
	return aMinor >= bMinor
}

// measuredOf returns the measured string for a tool: first line of the raw
// output truncated to 120 chars, or "missing" (bash ${measured:0:120}).
func measuredOf(raw string) string {
	line, _, _ := strings.Cut(raw, "\n")
	if len(line) > 120 {
		line = line[:120]
	}
	if line == "" {
		return "missing"
	}
	return line
}

// evalTool evaluates one regular (non-special) tool check.
func evalTool(id string, tm toolMeasurement) Check {
	def := toolDefs[id]
	var c Check
	pair := firstPair(tm.raw)
	measured := measuredOf(tm.raw)
	if !tm.present {
		setCheck(&c, id, "toolchain", "tool", "required", "failed", measured, "", def.base, def.remMiss, "")
		return c
	}
	switch def.kind {
	case kindPresence:
		setCheck(&c, id, "toolchain", "tool", "required", "passed", measured, pair, def.base, "", "")
	case kindMajor:
		switch {
		case pair == "":
			setCheck(&c, id, "toolchain", "tool", "required", "failed", measured, "", def.base, "cannot determine version — "+def.remMiss, "")
		case strings.Split(pair, ".")[0] == def.pin:
			setCheck(&c, id, "toolchain", "tool", "required", "passed", measured, pair, def.base, "", "")
		default:
			setCheck(&c, id, "toolchain", "tool", "required", "failed", measured, pair, def.base, def.remDrift, "")
		}
	case kindMajorMinor:
		switch {
		case pair == "":
			setCheck(&c, id, "toolchain", "tool", "required", "failed", measured, "", def.base, "cannot determine version — "+def.remMiss, "")
		case pair == def.pin:
			setCheck(&c, id, "toolchain", "tool", "required", "passed", measured, pair, def.base, "", "")
		default:
			setCheck(&c, id, "toolchain", "tool", "required", "failed", measured, pair, def.base, def.remDrift, "")
		}
	case kindMin:
		switch {
		case pair == "":
			setCheck(&c, id, "toolchain", "tool", "required", "failed", measured, "", def.base, "cannot determine version — "+def.remMiss, "")
		case cmpPairGe(pair, def.pin):
			setCheck(&c, id, "toolchain", "tool", "required", "passed", measured, pair, def.base, "", "")
		default:
			setCheck(&c, id, "toolchain", "tool", "required", "failed", measured, pair, def.base, def.remDrift, "")
		}
	}
	return c
}

func evalTools(o *Options, m *measurements) []Check {
	byID := map[string]Check{}
	var out []Check
	for _, id := range toolIDs {
		var c Check
		switch id {
		case "T-PNPM-PIN":
			c = evalPnpmPin(o, m, byID["T-PNPM"])
		case "T-DOCKER-COMPOSE":
			c = evalDockerCompose(o)
		case "T-DOCKER-DAEMON":
			c = evalDockerDaemon(o, m)
		default:
			c = evalTool(id, o.toolInput(id))
		}
		byID[id] = c
		out = append(out, c)
	}
	return out
}

func evalDockerCompose(o *Options) Check {
	var c Check
	present, raw := false, ""
	if o.FixtureDir != "" {
		if f := o.fixtureLines("docker-compose", 5); f != "" {
			present, raw = true, f
		} else if _, err := os.Stat(filepath.Join(o.FixtureDir, "docker-compose.absent")); err == nil {
			present = false
		} else {
			return evalComposeReal()
		}
	} else {
		return evalComposeReal()
	}
	pair := firstPair(raw)
	measured := measuredOf(raw)
	if !present {
		setCheck(&c, "T-DOCKER-COMPOSE", "toolchain", "tool", "required", "failed", measured, "", composeBase, remCompose, "")
	} else if pair == "" {
		setCheck(&c, "T-DOCKER-COMPOSE", "toolchain", "tool", "required", "failed", measured, "", composeBase, "cannot determine version — "+remCompose, "")
	} else if majorOf(pair) >= 2 {
		setCheck(&c, "T-DOCKER-COMPOSE", "toolchain", "tool", "required", "passed", measured, pair, composeBase, "", "")
	} else {
		setCheck(&c, "T-DOCKER-COMPOSE", "toolchain", "tool", "required", "failed", measured, pair, composeBase, remComposeVer, "")
	}
	return c
}

// evalComposeReal probes the real compose plugin (bash: docker CLI required,
// output must contain "Compose version"; missing CLI -> skipped).
func evalComposeReal() Check {
	var c Check
	if _, err := exec.LookPath("docker"); err != nil {
		setCheck(&c, "T-DOCKER-COMPOSE", "toolchain", "tool", "required", "skipped",
			"docker CLI missing", "", "v2.x", "", reasonCompose)
		return c
	}
	raw := composeVersionOutput()
	present := strings.Contains(raw, "Compose version")
	pair := firstPair(raw)
	measured := measuredOf(raw)
	if !present {
		setCheck(&c, "T-DOCKER-COMPOSE", "toolchain", "tool", "required", "failed", measured, "", composeBase, remCompose, "")
	} else if pair == "" {
		setCheck(&c, "T-DOCKER-COMPOSE", "toolchain", "tool", "required", "failed", measured, "", composeBase, "cannot determine version — "+remCompose, "")
	} else if majorOf(pair) >= 2 {
		setCheck(&c, "T-DOCKER-COMPOSE", "toolchain", "tool", "required", "passed", measured, pair, composeBase, "", "")
	} else {
		setCheck(&c, "T-DOCKER-COMPOSE", "toolchain", "tool", "required", "failed", measured, pair, composeBase, remComposeVer, "")
	}
	return c
}

// composeVersionOutput runs `docker compose version` (first 5 lines, 15s cap;
// the CLI version query does not touch the daemon socket).
func composeVersionOutput() string {
	tm := measureTool(toolDef{bin: "docker", verCmd: "docker compose version"})
	return tm.raw
}

func majorOf(pair string) int {
	maj, _, _ := strings.Cut(pair, ".")
	n, _ := strconv.Atoi(maj)
	return n
}

func evalDockerDaemon(o *Options, m *measurements) Check {
	var c Check
	if !o.CheckDockerDaemon {
		setCheck(&c, "T-DOCKER-DAEMON", "docker daemon", "tool", "advisory", "skipped",
			"disabled (opt-in)", "", "reachable", "", reasonDockerOptIn)
	} else if m.dockerReachable {
		setCheck(&c, "T-DOCKER-DAEMON", "docker daemon", "tool", "advisory", "passed", "reachable", "", "reachable", "", "")
	} else {
		setCheck(&c, "T-DOCKER-DAEMON", "docker daemon", "tool", "advisory", "warn", "unreachable", "", "reachable", reasonDaemonStart, "")
	}
	return c
}

var pnpmPinRe = regexp.MustCompile(`"packageManager"[[:space:]]*:[[:space:]]*"[^"]*"`)
var lastQuotedRe = regexp.MustCompile(`"([^"]+)"[[:space:]]*$`)

func evalPnpmPin(o *Options, m *measurements, pnpmCheck Check) Check {
	var c Check
	if pnpmCheck.Status != "passed" {
		setCheck(&c, "T-PNPM-PIN", "pnpm pin (packageManager)", "tool", "required", "skipped",
			"pnpm missing", "", "== packageManager field", "", reasonPinPnpm)
		return c
	}
	pkg := filepath.Join(m.repoPath, "package.json")
	data, err := os.ReadFile(pkg)
	if err != nil {
		setCheck(&c, "T-PNPM-PIN", "pnpm pin (packageManager)", "tool", "required", "skipped",
			"package.json absent", "", "== packageManager field", "", reasonPinNoPkg)
		return c
	}
	pm := ""
	if match := pnpmPinRe.Find(data); len(match) > 0 {
		if sub := lastQuotedRe.FindStringSubmatch(string(match)); len(sub) == 2 {
			pm = sub[1]
		}
	}
	if pm == "" || !strings.HasPrefix(pm, "pnpm@") {
		setCheck(&c, "T-PNPM-PIN", "pnpm pin (packageManager)", "tool", "required", "skipped",
			"field missing or malformed", "", "== packageManager field", "",
			fmt.Sprintf(reasonPinMalformed, pkg))
		return c
	}
	pinver := strings.TrimPrefix(pm, "pnpm@")
	if pnpmCheck.Measured == pinver {
		setCheck(&c, "T-PNPM-PIN", "pnpm pin (packageManager)", "tool", "required", "passed",
			"pnpm "+pnpmCheck.Measured+" == pin "+pinver, "", "== packageManager field", "", "")
	} else {
		setCheck(&c, "T-PNPM-PIN", "pnpm pin (packageManager)", "tool", "required", "failed",
			"pnpm "+pnpmCheck.Measured+" != pin "+pinver, "", "== packageManager field",
			fmt.Sprintf(remPinDrift, pinver), "")
	}
	return c
}
