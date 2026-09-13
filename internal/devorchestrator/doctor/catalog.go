package doctor

// The check catalog: ids, categories, severities, baselines and remediation
// strings are a Go port of ops/doctor.sh (T0000). ops/doctor-checks.md is the
// contract; every string below must match it and the reference implementation
// byte-for-byte — they are asserted against the live doctor.sh in the parity
// test. Do not "improve" the wording here without updating the contract
// first (ops/doctor-checks.md §8 baseline-change process).

// Check ids in canonical order (contract §7: ENV first, then RES, then T).
var envIDs = []string{"ENV-OS-KERNEL", "ENV-OS-DISTRO", "ENV-OS-ARCH", "ENV-WSL"}

var resIDs = []string{
	"RES-CPU", "RES-CPU-TIER", "RES-RAM", "RES-RAM-TIER", "RES-SWAP", "RES-SWAP-TIER",
	"RES-DISK", "RES-DISK-TIER", "RES-FD", "RES-DOCKER-DISK", "RES-REPO-FS",
	"RES-BWRAP", "RES-SOCAT",
}

var toolIDs = []string{
	"T-GIT", "T-GIT-LFS", "T-CLAUDE", "T-GO", "T-NODE", "T-PNPM", "T-PNPM-PIN",
	"T-PYTHON3", "T-UV", "T-DOCKER", "T-DOCKER-COMPOSE", "T-DOCKER-DAEMON",
	"T-JQ", "T-PSQL", "T-REDIS-CLI", "T-MAKE", "T-RG", "T-SHELLCHECK",
}

// toolKind: how the parsed version pair is compared against the pin.
type toolKind string

const (
	kindPresence   toolKind = "presence"
	kindMajor      toolKind = "major"
	kindMajorMinor toolKind = "majorminor"
	kindMin        toolKind = "min"
)

// toolDef mirrors the tool_def rows of ops/doctor.sh.
type toolDef struct {
	bin      string // binary probed by command -v
	verCmd   string // version command (run via bash -c, first 5 lines kept)
	kind     toolKind
	pin      string // baseline version pair ("" for presence)
	base     string // baseline text shown in output
	fix      string // fixture file name (also the doctor.sh TOOL_FIX key)
	remMiss  string // remediation when missing
	remDrift string // remediation when the version is outside the baseline
}

var toolDefs = map[string]toolDef{
	"T-GIT": {
		bin: "git", verCmd: "git --version", kind: kindMajor, pin: "2", base: "2.x", fix: "git",
		remMiss:  "sudo apt-get install -y git",
		remDrift: "restore git 2.x — major-version drift requires Supervisor sign-off (docs/64 §4)",
	},
	"T-GIT-LFS": {
		bin: "git-lfs", verCmd: "git lfs version", kind: kindMajor, pin: "3", base: "3.x", fix: "git-lfs",
		remMiss:  "sudo apt-get install -y git-lfs && git lfs install",
		remDrift: "restore git-lfs 3.x — major-version drift requires Supervisor sign-off (docs/64 §4)",
	},
	"T-CLAUDE": {
		bin: "claude", verCmd: "claude --version", kind: kindPresence, base: "recorded (manual upgrade policy)", fix: "claude",
		remMiss: "install Claude Code stable: npm install -g @anthropic-ai/claude-code (or the Ubuntu apt stable channel)",
	},
	"T-GO": {
		bin: "go", verCmd: "go version", kind: kindMajorMinor, pin: "1.27", base: "1.27.x", fix: "go",
		remMiss:  "install Go 1.27.x: https://go.dev/doc/install (extract to /usr/local)",
		remDrift: "restore go 1.27.x — major-version drift requires Supervisor sign-off (docs/64 §4)",
	},
	"T-NODE": {
		bin: "node", verCmd: "node --version", kind: kindMajor, pin: "24", base: "24.x (LTS)", fix: "node",
		remMiss:  "install Node.js 24 LTS (NodeSource or nvm)",
		remDrift: "restore Node.js 24.x — major-version drift requires Supervisor sign-off (docs/64 §4)",
	},
	"T-PNPM": {
		bin: "pnpm", verCmd: "pnpm --version", kind: kindPresence, base: "packageManager pin (recorded)", fix: "pnpm",
		remMiss: "corepack enable && corepack prepare --activate (or npm install -g pnpm@<packageManager pin>)",
	},
	"T-PYTHON3": {
		bin: "python3", verCmd: "python3 --version", kind: kindMin, pin: "3.12", base: ">= 3.12", fix: "python3",
		remMiss:  "install Python 3.12+ (Ubuntu package or deadsnakes PPA)",
		remDrift: "restore Python >= 3.12 (docs/64 §4)",
	},
	"T-UV": {
		bin: "uv", verCmd: "uv --version", kind: kindPresence, base: "recorded", fix: "uv",
		remMiss: "install uv: curl -LsSf https://astral.sh/uv/install.sh | sh",
	},
	"T-DOCKER": {
		bin: "docker", verCmd: "docker --version", kind: kindPresence, base: "Engine + Compose v2 (recorded)", fix: "docker",
		remMiss: "install Docker Engine from https://docs.docker.com/engine/install/ubuntu/",
	},
	"T-JQ": {
		bin: "jq", verCmd: "jq --version", kind: kindMajor, pin: "1", base: "1.x", fix: "jq",
		remMiss:  "sudo apt-get install -y jq",
		remDrift: "restore jq 1.x — major-version drift requires Supervisor sign-off (docs/64 §4)",
	},
	"T-PSQL": {
		bin: "psql", verCmd: "psql --version", kind: kindMajor, pin: "16", base: "16.x", fix: "psql",
		remMiss:  "sudo apt-get install -y postgresql-client (16.x on noble)",
		remDrift: "restore psql 16.x — major-version drift requires Supervisor sign-off (docs/64 §4)",
	},
	"T-REDIS-CLI": {
		bin: "redis-cli", verCmd: "redis-cli --version", kind: kindMajor, pin: "7", base: "7.x", fix: "redis-cli",
		remMiss:  "sudo apt-get install -y redis-tools (7.x)",
		remDrift: "restore redis-cli 7.x — major-version drift requires Supervisor sign-off (docs/64 §4)",
	},
	"T-MAKE": {
		bin: "make", verCmd: "make --version", kind: kindMajor, pin: "4", base: "4.x", fix: "make",
		remMiss:  "sudo apt-get install -y make",
		remDrift: "restore GNU Make 4.x — major-version drift requires Supervisor sign-off (docs/64 §4)",
	},
	"T-RG": {
		bin: "rg", verCmd: "rg --version", kind: kindPresence, base: "present (binary: rg)", fix: "rg",
		remMiss: `sudo apt-get install -y ripgrep — note: the binary is named "rg", not "ripgrep"`,
	},
	"T-SHELLCHECK": {
		bin: "shellcheck", verCmd: "shellcheck --version", kind: kindPresence, base: "recorded", fix: "shellcheck",
		remMiss: "sudo apt-get install -y shellcheck",
	},
}

// Fixed remediation/note strings for env and resource checks (contract §7).
const (
	remWindowsNative = "Windows Native is not a supported canonical environment. Use WSL2 with Ubuntu 24.04 and keep the repo on the Linux filesystem, e.g. /home/<user>/src/post (never /mnt/c/...). See docs/64_UBUNTU_DEV_ENV.md §1."
	remOtherKernel   = "unsupported OS kernel — run on Ubuntu 24.04 LTS amd64 (docs/64_UBUNTU_DEV_ENV.md §1)"
	remDistro        = "install Ubuntu 24.04 LTS (noble), or WSL2 with Ubuntu 24.04 (docs/64_UBUNTU_DEV_ENV.md §1)"
	remArch          = "canonical architecture is amd64/x86_64 (docs/64_UBUNTU_DEV_ENV.md §1)"
	remWSL1          = "WSL2 required — run: wsl --set-version <distro> 2 (docs/64_UBUNTU_DEV_ENV.md §1)"

	remResUnmeasurable = "unable to measure — verify a standard Linux /proc environment"
	remCPU             = "provision >= 8 vCPU (docs/64 §2)"
	remCPUTier         = "recommended >= 16 vCPU for Supervisor + 3–4 Workers (docs/64 §2)"
	remRAM             = "provision >= 32 GiB RAM (docs/64 §2)"
	remRAMTier         = "recommended >= 64 GiB RAM (docs/64 §2)"
	remSwap            = "provision >= 8 GiB swap (docs/64 §2)"
	remSwapTier        = "recommended >= 16 GiB swap (docs/64 §2)"
	remDisk            = "free >= 20 GiB on the repo filesystem (docs/64 §2/§6)"
	remDiskTier        = "recommended >= 100 GiB free (docs/64 §2: 250 GB NVMe class)"
	remFD              = "raise the limit now: ulimit -n 65535; persist via /etc/security/limits.conf (nofile) (docs/64 §6)"
	remDockerDisk      = "ensure the docker data-root has >= 100 GiB free; prune via 'rddev env gc' (docs/64 §6)"
	remDockerDiskNoM   = "unable to measure docker data-root free space (docs/64 §6)"
	reasonDockerOptIn  = "disabled by default — probing the Docker daemon needs socket access; enable with --check-docker-daemon"
	reasonDockerDown   = "docker daemon unreachable"
	remRepoFS          = "move the repo onto the Linux filesystem, e.g. /home/<user>/src/post — never /mnt/c/... (docs/64_UBUNTU_DEV_ENV.md §1)"
	reasonRepoFS       = "only checked under WSL"
	remBwrap           = "sudo apt-get install -y bubblewrap (Claude Code sandbox prerequisite, docs/64 §3)"
	remSocat           = "sudo apt-get install -y socat (Claude Code sandbox prerequisite, docs/64 §3)"

	composeBase   = "v2 plugin lineage (major >= 2)"
	remCompose    = "sudo apt-get install -y docker-compose-plugin (Compose v2; legacy docker-compose v1 is not canonical)"
	remComposeVer = "restore the Docker Compose v2 plugin lineage (docker compose, major >= 2) — legacy docker-compose v1 is not canonical (docs/64 §4)"
	reasonCompose = "depends on T-DOCKER (docker CLI missing)"

	reasonDaemonStart = "start Docker: sudo systemctl enable --now docker (host privileges — never in a Worker context)"

	reasonPinPnpm      = "depends on T-PNPM (pnpm missing)"
	reasonPinNoPkg     = "package.json arrives with the T0002 scaffold; pin check deferred"
	reasonPinMalformed = "packageManager field is missing or not a pnpm pin in %s"
	remPinDrift        = "align pnpm with the pin: corepack enable && corepack prepare pnpm@%s --activate — pin drift requires Supervisor sign-off"
)

// Resource thresholds in kB (contract §9).
const (
	giB             = 1048576
	minCPU          = 8
	minCPUTier      = 16
	minRAMKB        = 32 * giB
	minRAMTierKB    = 64 * giB
	minSwapKB       = 8 * giB
	minSwapTierKB   = 16 * giB
	minDiskKB       = 20 * giB
	minDiskTierKB   = 100 * giB
	minFD           = 65535
	minDockerDiskKB = 100 * giB
)
