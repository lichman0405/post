#!/usr/bin/env bash
#
# ops/doctor.sh — POST canonical environment preflight (reference implementation).
#
# Contract  : ops/doctor-checks.md         (check ids, severity, remediation; the spec
#                                           T0009 implements in Go for `rddev doctor`)
# Schema    : ops/doctor-output.schema.json
# Baselines : docs/64_UBUNTU_DEV_ENV.md    (pinned toolchain versions)
#
# Deterministic: same inputs -> same bytes out. No timestamps, no randomness,
# fixed check order, LC_ALL=C. Filesystem-anchored checks (disk free,
# package.json pin, WSL repo location) use the invocation directory — run this
# from the repo root; `rddev doctor` will do the same.
#
# Stable exit codes:
#   0  all required checks passed (advisory warnings allowed)
#   1  toolchain failure: a required tool is missing or its version is outside
#      the pinned baseline
#   2  unsupported environment: non-Linux / Windows Native / non-Ubuntu 24.04 /
#      non-amd64
#   3  usage error
#
# Fixture hook (test-only): --fixture DIR overrides measured inputs so decision
# logic can be exercised against controlled inputs without touching the host
# toolchain. See ops/doctor-checks.md §6 for the exact fixture contract.
set -u
export LC_ALL=C

readonly SCRIPT_NAME="ops/doctor.sh"
readonly CONTRACT_REF="ops/doctor-checks.md"
readonly SCHEMA_VERSION=1

# ------------------------------------------------------------------- options
JSON_MODE=0
CHECK_DOCKER_DAEMON=0
FIXTURE_DIR=""

usage() {
  cat <<'EOF'
Usage: ops/doctor.sh [--json] [--fixture DIR] [--check-docker-daemon] [-h|--help]

POST canonical environment preflight (Ubuntu 24.04 LTS amd64, see docs/64_UBUNTU_DEV_ENV.md).
Run from the repo root.

Options:
  --json                  machine-readable output (JSON, ops/doctor-output.schema.json)
  --fixture DIR           test-only: override measured inputs from fixture files
                          (contract: ops/doctor-checks.md §6)
  --check-docker-daemon   also probe the Docker daemon (docker info) and check free
                          space on the docker data-root (docs/64 §6). Off by default:
                          probing the daemon needs Docker socket access, which is
                          reserved on Worker machines.
  -h, --help              show this help

Exit codes: 0 = all required checks passed; 1 = toolchain failure (missing tool or
version outside pinned baseline); 2 = unsupported environment (non-Linux, Windows
Native, non-Ubuntu 24.04, non-amd64); 3 = usage error.
EOF
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --json) JSON_MODE=1 ;;
      --fixture)
        [[ $# -ge 2 ]] || { echo "error: --fixture requires a directory" >&2; usage >&2; exit 3; }
        FIXTURE_DIR="$2"; shift ;;
      --fixture=*)
        FIXTURE_DIR="${1#--fixture=}"
        [[ -n "$FIXTURE_DIR" ]] || { echo "error: --fixture requires a directory" >&2; usage >&2; exit 3; } ;;
      --check-docker-daemon) CHECK_DOCKER_DAEMON=1 ;;
      -h|--help) usage; exit 0 ;;
      *) echo "error: unknown option: $1" >&2; usage >&2; exit 3 ;;
    esac
    shift
  done
  if [[ -n "$FIXTURE_DIR" ]] && [[ ! -d "$FIXTURE_DIR" ]]; then
    echo "error: fixture directory not found: $FIXTURE_DIR" >&2
    exit 3
  fi
}

# ------------------------------------------------------------------- check catalog
ENV_IDS=(ENV-OS-KERNEL ENV-OS-DISTRO ENV-OS-ARCH ENV-WSL)
RES_IDS=(RES-CPU RES-CPU-TIER RES-RAM RES-RAM-TIER RES-SWAP RES-SWAP-TIER
         RES-DISK RES-DISK-TIER RES-FD RES-DOCKER-DISK RES-REPO-FS
         RES-BWRAP RES-SOCAT)
TOOL_IDS=(T-GIT T-GIT-LFS T-CLAUDE T-GO T-NODE T-PNPM T-PNPM-PIN T-PYTHON3
          T-UV T-DOCKER T-DOCKER-COMPOSE T-DOCKER-DAEMON T-JQ T-PSQL
          T-REDIS-CLI T-MAKE T-RG T-SHELLCHECK)

# Per-check result state (all ids are initialized in init_checks).
declare -A CK_NAME CK_CATEGORY CK_SEVERITY CK_STATUS CK_MEASURED CK_VERSION \
           CK_BASELINE CK_REMEDIATION CK_REASON

init_checks() {
  local id
  for id in "${ENV_IDS[@]}" "${RES_IDS[@]}" "${TOOL_IDS[@]}"; do
    CK_NAME[$id]="$id"; CK_CATEGORY[$id]=""; CK_SEVERITY[$id]=""; CK_STATUS[$id]=""
    CK_MEASURED[$id]=""; CK_VERSION[$id]=""; CK_BASELINE[$id]=""
    CK_REMEDIATION[$id]=""; CK_REASON[$id]=""
  done
}

set_check() { # id name category severity status measured version baseline remediation reason
  local id="$1"
  CK_NAME[$id]="$2"; CK_CATEGORY[$id]="$3"; CK_SEVERITY[$id]="$4"; CK_STATUS[$id]="$5"
  CK_MEASURED[$id]="$6"; CK_VERSION[$id]="$7"; CK_BASELINE[$id]="$8"
  CK_REMEDIATION[$id]="$9"; CK_REASON[$id]="${10}"
}

# Tool metadata. TOOL_KIND: presence | major | majorminor | min
declare -A TOOL_BIN TOOL_VERCMD TOOL_KIND TOOL_PIN TOOL_BASE TOOL_FIX \
           TOOL_REM_MISS TOOL_REM_DRIFT

tool_def() { # id bin vercmd kind pin base fix rem_miss rem_drift
  local id="$1"
  TOOL_BIN[$id]="$2"; TOOL_VERCMD[$id]="$3"; TOOL_KIND[$id]="$4"
  TOOL_PIN[$id]="$5"; TOOL_BASE[$id]="$6"; TOOL_FIX[$id]="$7"
  TOOL_REM_MISS[$id]="$8"; TOOL_REM_DRIFT[$id]="$9"
}

tool_def T-GIT          git     'git --version'        major      2    '2.x'              git             'sudo apt-get install -y git'                                                             'restore git 2.x — major-version drift requires Supervisor sign-off (docs/64 §4)'
tool_def T-GIT-LFS      git-lfs 'git lfs version'      major      3    '3.x'              git-lfs         'sudo apt-get install -y git-lfs && git lfs install'                                     'restore git-lfs 3.x — major-version drift requires Supervisor sign-off (docs/64 §4)'
tool_def T-CLAUDE       claude  'claude --version'     presence   ''   'recorded (manual upgrade policy)' claude  'install Claude Code stable: npm install -g @anthropic-ai/claude-code (or the Ubuntu apt stable channel)' ''
tool_def T-GO           go      'go version'           majorminor 1.27 '1.27.x'           go              'install Go 1.27.x: https://go.dev/doc/install (extract to /usr/local)'                  'restore go 1.27.x — major-version drift requires Supervisor sign-off (docs/64 §4)'
tool_def T-NODE         node    'node --version'       major      24   '24.x (LTS)'       node            'install Node.js 24 LTS (NodeSource or nvm)'                                             'restore Node.js 24.x — major-version drift requires Supervisor sign-off (docs/64 §4)'
tool_def T-PNPM         pnpm    'pnpm --version'       presence   ''   'packageManager pin (recorded)' pnpm    'corepack enable && corepack prepare --activate (or npm install -g pnpm@<packageManager pin>)' ''
tool_def T-PYTHON3      python3 'python3 --version'    min        3.12 '>= 3.12'          python3         'install Python 3.12+ (Ubuntu package or deadsnakes PPA)'                                 'restore Python >= 3.12 (docs/64 §4)'
tool_def T-UV           uv      'uv --version'         presence   ''   'recorded'         uv              'install uv: curl -LsSf https://astral.sh/uv/install.sh | sh'                            ''
tool_def T-DOCKER       docker  'docker --version'     presence   ''   'Engine + Compose v2 (recorded)' docker   'install Docker Engine from https://docs.docker.com/engine/install/ubuntu/'             ''
tool_def T-JQ           jq      'jq --version'         major      1    '1.x'              jq              'sudo apt-get install -y jq'                                                            'restore jq 1.x — major-version drift requires Supervisor sign-off (docs/64 §4)'
tool_def T-PSQL         psql    'psql --version'       major      16   '16.x'             psql            'sudo apt-get install -y postgresql-client (16.x on noble)'                              'restore psql 16.x — major-version drift requires Supervisor sign-off (docs/64 §4)'
tool_def T-REDIS-CLI    redis-cli 'redis-cli --version' major    7    '7.x'              redis-cli       'sudo apt-get install -y redis-tools (7.x)'                                             'restore redis-cli 7.x — major-version drift requires Supervisor sign-off (docs/64 §4)'
tool_def T-MAKE         make    'make --version'       major      4    '4.x'              make            'sudo apt-get install -y make'                                                          'restore GNU Make 4.x — major-version drift requires Supervisor sign-off (docs/64 §4)'
tool_def T-RG           rg      'rg --version'         presence   ''   'present (binary: rg)' rg           'sudo apt-get install -y ripgrep — note: the binary is named "rg", not "ripgrep"'      ''
tool_def T-SHELLCHECK   shellcheck 'shellcheck --version' presence   ''   'recorded'        shellcheck      'sudo apt-get install -y shellcheck'                                                    ''

# ------------------------------------------------------------------- measurement
KERNEL=""; ARCH=""; DISTRO_ID=""; DISTRO_VERSION_ID=""; DISTRO_PRETTY=""
CODENAME=""; WSL_KIND="none"; REPO_PATH=""; REPO_FS_MOUNT=""
RES_NPROC=""; RES_MEM_KB=""; RES_SWAP_KB=""; RES_FD=""; RES_DISK_KB=""
DOCKER_REACHABLE=0; DOCKER_ROOT=""; RES_DOCKER_DISK_KB=""

fixture_val() { # file -> prints first line, or nothing
  [[ -n "$FIXTURE_DIR" && -r "$FIXTURE_DIR/$1" ]] || return 0
  head -n1 "$FIXTURE_DIR/$1" 2>/dev/null | tr -d '\r'
}

measure_env() {
  KERNEL="$(uname -s 2>/dev/null || true)"
  ARCH="$(uname -m 2>/dev/null || true)"
  if [[ -r /etc/os-release ]]; then
    local k v
    while IFS='=' read -r k v || [[ -n "$k" ]]; do
      case "$k" in
        ID)               DISTRO_ID="${v//\"/}" ;;
        VERSION_ID)       DISTRO_VERSION_ID="${v//\"/}" ;;
        PRETTY_NAME)      DISTRO_PRETTY="${v//\"/}" ;;
        VERSION_CODENAME) CODENAME="${v//\"/}" ;;
      esac
    done < /etc/os-release
  fi
  WSL_KIND="none"
  if [[ -r /proc/version ]] && grep -qi microsoft /proc/version; then
    if grep -qi 'WSL2' /proc/version; then WSL_KIND="wsl2"; else WSL_KIND="wsl1"; fi
  fi
  REPO_PATH="$(pwd -P 2>/dev/null || pwd)"
  REPO_FS_MOUNT="$(df -Pk "$REPO_PATH" 2>/dev/null | awk 'NR==2 {print $6}')"

  local fv
  fv="$(fixture_val os.kernel)";       [[ -n "$fv" ]] && KERNEL="$fv"
  fv="$(fixture_val os.arch)";         [[ -n "$fv" ]] && ARCH="$fv"
  fv="$(fixture_val os.id)";           [[ -n "$fv" ]] && DISTRO_ID="$fv"
  fv="$(fixture_val os.version_id)";   [[ -n "$fv" ]] && DISTRO_VERSION_ID="$fv"
  fv="$(fixture_val os.pretty)";       [[ -n "$fv" ]] && DISTRO_PRETTY="$fv"
  fv="$(fixture_val os.codename)";     [[ -n "$fv" ]] && CODENAME="$fv"
  fv="$(fixture_val os.wsl)";          [[ -n "$fv" ]] && WSL_KIND="$fv"
  fv="$(fixture_val os.repo_path)";    [[ -n "$fv" ]] && REPO_PATH="$fv"
}

measure_resources() {
  RES_NPROC="$(nproc 2>/dev/null || true)"
  RES_MEM_KB="$(awk '/^MemTotal:/ {print $2; exit}' /proc/meminfo 2>/dev/null || true)"
  RES_SWAP_KB="$(awk '/^SwapTotal:/ {print $2; exit}' /proc/meminfo 2>/dev/null || true)"
  RES_FD="$(ulimit -n 2>/dev/null || true)"
  RES_DISK_KB="$(df -Pk "$REPO_PATH" 2>/dev/null | awk 'NR==2 {print $4}')"

  local fv
  fv="$(fixture_val res.nproc)";          [[ -n "$fv" ]] && RES_NPROC="$fv"
  fv="$(fixture_val res.mem_total_kb)";   [[ -n "$fv" ]] && RES_MEM_KB="$fv"
  fv="$(fixture_val res.swap_total_kb)";  [[ -n "$fv" ]] && RES_SWAP_KB="$fv"
  fv="$(fixture_val res.fd_limit)";       [[ -n "$fv" ]] && RES_FD="$fv"
  fv="$(fixture_val res.disk_free_kb)";   [[ -n "$fv" ]] && RES_DISK_KB="$fv"
}

measure_docker_daemon() {
  # Only called when --check-docker-daemon is active (needs Docker socket access;
  # default off — Workers must not probe the daemon).
  local fv
  fv="$(fixture_val res.docker_reachable)"
  if [[ -n "$fv" ]]; then
    [[ "$fv" == "true" ]] && DOCKER_REACHABLE=1 || DOCKER_REACHABLE=0
  else
    DOCKER_REACHABLE=0
    timeout 10 docker info >/dev/null 2>&1 && DOCKER_REACHABLE=1
  fi
  fv="$(fixture_val res.docker_root_free_kb)"
  if [[ -n "$fv" ]]; then
    RES_DOCKER_DISK_KB="$fv"
  elif [[ "$DOCKER_REACHABLE" -eq 1 ]]; then
    DOCKER_ROOT="$(timeout 10 docker info --format '{{.DockerRootDir}}' 2>/dev/null | head -n1 || true)"
    if [[ -n "$DOCKER_ROOT" ]]; then
      RES_DOCKER_DISK_KB="$(df -Pk "$DOCKER_ROOT" 2>/dev/null | awk 'NR==2 {print $4}')"
    fi
  fi
}

# ------------------------------------------------------------------- helpers
first_pair() { printf '%s' "$1" | grep -oE '[0-9]+\.[0-9]+' | head -n1; }

# cmp_pair_ge a b -> true iff numeric pair a >= pair b (e.g. "3.12" >= "3.12")
cmp_pair_ge() {
  local a_maj="${1%%.*}" a_min="${1#*.}" b_maj="${2%%.*}" b_min="${2#*.}"
  [[ "$1" != *.* ]] && a_min="0"
  [[ "$2" != *.* ]] && b_min="0"
  (( 10#${a_maj:-0} > 10#${b_maj:-0} )) && return 0
  (( 10#${a_maj:-0} < 10#${b_maj:-0} )) && return 1
  (( 10#${a_min:-0} >= 10#${b_min:-0} ))
}

json_escape() {
  local s="$1"
  s=${s//\\/\\\\}; s=${s//\"/\\\"}
  s=${s//$'\n'/\\n}; s=${s//$'\r'/\\r}; s=${s//$'\t'/\\t}
  printf '%s' "$s"
}

kb_to_gib() { printf '%d' "$(( ${1:-0} / 1048576 ))"; }

# ------------------------------------------------------------------- evaluation
eval_env() {
  # ENV-OS-KERNEL (required; failure -> unsupported verdict)
  local kern="$KERNEL"
  if [[ "${OS:-}" == "Windows_NT" ]]; then
    kern="Windows_NT"
  fi
  case "$kern" in
    Linux)
      set_check ENV-OS-KERNEL 'OS kernel' env required passed "kernel: $KERNEL" "" "Linux" "" ""
      ;;
    MINGW*|MSYS*|CYGWIN*|Windows_NT)
      set_check ENV-OS-KERNEL 'OS kernel' env required failed "kernel: $kern (Windows Native)" "" "Linux" \
        "Windows Native is not a supported canonical environment. Use WSL2 with Ubuntu 24.04 and keep the repo on the Linux filesystem, e.g. /home/<user>/src/post (never /mnt/c/...). See docs/64_UBUNTU_DEV_ENV.md §1." ""
      ;;
    *)
      set_check ENV-OS-KERNEL 'OS kernel' env required failed "kernel: $kern" "" "Linux" \
        "unsupported OS kernel — run on Ubuntu 24.04 LTS amd64 (docs/64_UBUNTU_DEV_ENV.md §1)" ""
      ;;
  esac

  # ENV-OS-DISTRO (required; failure -> unsupported verdict)
  if [[ "$DISTRO_ID" == "ubuntu" && "$DISTRO_VERSION_ID" == "24.04" ]]; then
    set_check ENV-OS-DISTRO 'OS distro' env required passed \
      "distro: ${DISTRO_PRETTY:-ubuntu} ${DISTRO_VERSION_ID} (${CODENAME:-})" "" "ubuntu 24.04 (noble)" "" ""
  else
    set_check ENV-OS-DISTRO 'OS distro' env required failed \
      "distro: ${DISTRO_ID:-unknown} ${DISTRO_VERSION_ID:-unknown}" "" "ubuntu 24.04 (noble)" \
      "install Ubuntu 24.04 LTS (noble), or WSL2 with Ubuntu 24.04 (docs/64_UBUNTU_DEV_ENV.md §1)" ""
  fi

  # ENV-OS-ARCH (required; failure -> unsupported verdict)
  if [[ "$ARCH" == "x86_64" ]]; then
    set_check ENV-OS-ARCH 'CPU architecture' env required passed "arch: $ARCH" "" "x86_64 (amd64)" "" ""
  else
    set_check ENV-OS-ARCH 'CPU architecture' env required failed "arch: ${ARCH:-unknown}" "" "x86_64 (amd64)" \
      "canonical architecture is amd64/x86_64 (docs/64_UBUNTU_DEV_ENV.md §1)" ""
  fi

  # ENV-WSL (advisory)
  if [[ "$WSL_KIND" == "wsl1" ]]; then
    set_check ENV-WSL 'WSL kind' env advisory warn "WSL: $WSL_KIND" "" "wsl2 or none" \
      "WSL2 required — run: wsl --set-version <distro> 2 (docs/64_UBUNTU_DEV_ENV.md §1)" ""
  else
    set_check ENV-WSL 'WSL kind' env advisory passed "WSL: $WSL_KIND" "" "wsl2 or none" "" ""
  fi
}

eval_resources() {
  # Generic numeric threshold checks.
  # src: nproc | mem_kb | swap_kb | fd_limit | disk_kb
  eval_res() { # id src min_kb_or_count severity(tier?) label_suffix rem base
    local id="$1" src="$2" min="$3" sev="$4" rem="$5" base="$6"
    local cur="" disp=""
    case "$src" in
      nproc)   cur="$RES_NPROC";   disp="$cur cores" ;;
      mem_kb)  cur="$RES_MEM_KB";  disp="$(kb_to_gib "$cur") GiB total" ;;
      swap_kb) cur="$RES_SWAP_KB"; disp="$(kb_to_gib "$cur") GiB total" ;;
      fd_limit) cur="$RES_FD";     disp="$cur" ;;
      disk_kb) cur="$RES_DISK_KB"; disp="$(kb_to_gib "$cur") GiB free" ;;
    esac
    if [[ -z "$cur" || ! "$cur" =~ ^[0-9]+$ ]]; then
      set_check "$id" 'system resource' resource "$sev" failed "unable to measure ($src)" "" "$base" \
        "unable to measure — verify a standard Linux /proc environment" ""
      return
    fi
    local ok=1
    (( cur >= min )) || ok=0
    if [[ "$ok" -eq 1 ]]; then
      set_check "$id" 'system resource' resource "$sev" passed "$disp" "" "$base" "" ""
    elif [[ "$sev" == "advisory" ]]; then
      set_check "$id" 'system resource' resource "$sev" warn "$disp" "" "$base" "$rem" ""
    else
      set_check "$id" 'system resource' resource "$sev" failed "$disp" "" "$base" "$rem" ""
    fi
  }

  eval_res RES-CPU       nproc     8            required "provision >= 8 vCPU (docs/64 §2)" ">= 8"
  eval_res RES-CPU-TIER  nproc     16           advisory "recommended >= 16 vCPU for Supervisor + 3–4 Workers (docs/64 §2)" ">= 16 (recommended)"
  eval_res RES-RAM       mem_kb    $((32*1048576))  required "provision >= 32 GiB RAM (docs/64 §2)" ">= 32 GiB"
  eval_res RES-RAM-TIER  mem_kb    $((64*1048576))  advisory "recommended >= 64 GiB RAM (docs/64 §2)" ">= 64 GiB (recommended)"
  eval_res RES-SWAP      swap_kb   $((8*1048576))   required "provision >= 8 GiB swap (docs/64 §2)" ">= 8 GiB"
  eval_res RES-SWAP-TIER swap_kb   $((16*1048576))  advisory "recommended >= 16 GiB swap (docs/64 §2)" ">= 16 GiB (recommended)"
  eval_res RES-DISK      disk_kb   $((20*1048576))  required "free >= 20 GiB on the repo filesystem (docs/64 §2/§6)" ">= 20 GiB free"
  eval_res RES-DISK-TIER disk_kb   $((100*1048576)) advisory "recommended >= 100 GiB free (docs/64 §2: 250 GB NVMe class)" ">= 100 GiB free (recommended)"
  eval_res RES-FD        fd_limit  65535        required "raise the limit now: ulimit -n 65535; persist via /etc/security/limits.conf (nofile) (docs/64 §6)" ">= 65535"

  # RES-DOCKER-DISK (required when measurable; needs --check-docker-daemon)
  if [[ "$CHECK_DOCKER_DAEMON" -ne 1 ]]; then
    set_check RES-DOCKER-DISK 'docker data-root free space' resource required skipped \
      "disabled (opt-in)" "" ">= 100 GiB free on docker data-root" "" \
      "disabled by default — probing the Docker daemon needs socket access; enable with --check-docker-daemon"
  elif [[ "$DOCKER_REACHABLE" -ne 1 ]]; then
    set_check RES-DOCKER-DISK 'docker data-root free space' resource required skipped \
      "daemon unreachable" "" ">= 100 GiB free on docker data-root" "" \
      "docker daemon unreachable"
  else
    local kb="$RES_DOCKER_DISK_KB" disp="unable to measure (docker data-root)"
    [[ -n "$kb" && "$kb" =~ ^[0-9]+$ ]] && disp="$(kb_to_gib "$kb") GiB free"
    if [[ -n "$kb" && "$kb" =~ ^[0-9]+$ ]] && (( kb >= 100*1048576 )); then
      set_check RES-DOCKER-DISK 'docker data-root free space' resource required passed "$disp" "" ">= 100 GiB free on docker data-root" "" ""
    elif [[ -n "$kb" && "$kb" =~ ^[0-9]+$ ]]; then
      set_check RES-DOCKER-DISK 'docker data-root free space' resource required failed "$disp" "" ">= 100 GiB free on docker data-root" \
        "ensure the docker data-root has >= 100 GiB free; prune via 'rddev env gc' (docs/64 §6)" ""
    else
      set_check RES-DOCKER-DISK 'docker data-root free space' resource required failed "$disp" "" ">= 100 GiB free on docker data-root" \
        "unable to measure docker data-root free space (docs/64 §6)" ""
    fi
  fi

  # RES-REPO-FS (required only under WSL: repo must live on the Linux filesystem)
  if [[ "$WSL_KIND" == "none" ]]; then
    set_check RES-REPO-FS 'repo filesystem (WSL)' resource required skipped \
      "not running under WSL" "" "repo not under /mnt/" "" "only checked under WSL"
  else
    case "$REPO_PATH" in
      /mnt/*)
        set_check RES-REPO-FS 'repo filesystem (WSL)' resource required failed \
          "repo path: $REPO_PATH" "" "repo not under /mnt/" \
          "move the repo onto the Linux filesystem, e.g. /home/<user>/src/post — never /mnt/c/... (docs/64_UBUNTU_DEV_ENV.md §1)" ""
        ;;
      *)
        set_check RES-REPO-FS 'repo filesystem (WSL)' resource required passed \
          "repo path: $REPO_PATH" "" "repo not under /mnt/" "" ""
        ;;
    esac
  fi

  # RES-BWRAP / RES-SOCAT (advisory: Claude Code sandbox prerequisites, docs/64 §3)
  eval_presence_res() { # id fixture-name bin name rem
    local id="$1" fix="$2" bin="$3" name="$4" rem="$5"
    local present=0 fv=""
    if [[ -n "$FIXTURE_DIR" ]]; then
      fv="$(fixture_val "res.$fix")"
      if [[ "$fv" == "present" ]]; then present=1; elif [[ "$fv" == "absent" ]]; then present=0
      else
        command -v "$bin" >/dev/null 2>&1 && present=1
      fi
    else
      command -v "$bin" >/dev/null 2>&1 && present=1
    fi
    if [[ "$present" -eq 1 ]]; then
      set_check "$id" 'sandbox prerequisite' resource advisory passed "$name: present" "" "present" "" ""
    else
      set_check "$id" 'sandbox prerequisite' resource advisory warn "$name: missing" "" "present" "$rem" ""
    fi
  }
  eval_presence_res RES-BWRAP bwrap bubblewrap bubblewrap \
    "sudo apt-get install -y bubblewrap (Claude Code sandbox prerequisite, docs/64 §3)"
  eval_presence_res RES-SOCAT socat socat socat \
    "sudo apt-get install -y socat (Claude Code sandbox prerequisite, docs/64 §3)"
}

# Real (non-fixture) measurement of one tool. Sets T_PRESENT / T_RAW.
T_PRESENT=0; T_RAW=""
measure_real_tool() {
  local id="$1"
  T_PRESENT=0; T_RAW=""
  if command -v "${TOOL_BIN[$id]}" >/dev/null 2>&1; then
    T_PRESENT=1
    T_RAW="$(timeout 15 bash -c "${TOOL_VERCMD[$id]}" 2>&1 | head -n5 || true)"
  fi
}

eval_tools() {
  local id kind pin raw_all pair measured present

  for id in "${TOOL_IDS[@]}"; do
    case "$id" in
      T-PNPM-PIN)      eval_pnpm_pin; continue ;;
      T-DOCKER-COMPOSE) eval_docker_compose; continue ;;
      T-DOCKER-DAEMON) eval_docker_daemon; continue ;;
    esac

    kind="${TOOL_KIND[$id]}"; pin="${TOOL_PIN[$id]}"
    present=0; raw_all=""
    if [[ -n "$FIXTURE_DIR" ]]; then
      if [[ -r "$FIXTURE_DIR/${TOOL_FIX[$id]}" ]]; then
        present=1
        raw_all="$(head -n5 "$FIXTURE_DIR/${TOOL_FIX[$id]}" 2>/dev/null | tr -d '\r')"
      elif [[ -r "$FIXTURE_DIR/${TOOL_FIX[$id]}.absent" ]]; then
        present=0
      else
        measure_real_tool "$id"
        present=$T_PRESENT; raw_all="$T_RAW"
      fi
    else
      measure_real_tool "$id"
      present=$T_PRESENT; raw_all="$T_RAW"
    fi

    pair="$(first_pair "$raw_all")"
    measured="$(printf '%s' "$raw_all" | head -n1)"
    measured="${measured:0:120}"
    [[ -z "$measured" ]] && measured="missing"

    if [[ "$present" -eq 0 ]]; then
      set_check "$id" 'toolchain' tool required failed "$measured" "" "${TOOL_BASE[$id]}" "${TOOL_REM_MISS[$id]}" ""
      continue
    fi

    case "$kind" in
      presence)
        set_check "$id" 'toolchain' tool required passed "$measured" "$pair" "${TOOL_BASE[$id]}" "" ""
        ;;
      major)
        if [[ -z "$pair" ]]; then
          set_check "$id" 'toolchain' tool required failed "$measured" "" "${TOOL_BASE[$id]}" \
            "cannot determine version — ${TOOL_REM_MISS[$id]}" ""
        elif [[ "${pair%%.*}" == "$pin" ]]; then
          set_check "$id" 'toolchain' tool required passed "$measured" "$pair" "${TOOL_BASE[$id]}" "" ""
        else
          set_check "$id" 'toolchain' tool required failed "$measured" "$pair" "${TOOL_BASE[$id]}" "${TOOL_REM_DRIFT[$id]}" ""
        fi
        ;;
      majorminor)
        if [[ -z "$pair" ]]; then
          set_check "$id" 'toolchain' tool required failed "$measured" "" "${TOOL_BASE[$id]}" \
            "cannot determine version — ${TOOL_REM_MISS[$id]}" ""
        elif [[ "$pair" == "$pin" ]]; then
          set_check "$id" 'toolchain' tool required passed "$measured" "$pair" "${TOOL_BASE[$id]}" "" ""
        else
          set_check "$id" 'toolchain' tool required failed "$measured" "$pair" "${TOOL_BASE[$id]}" "${TOOL_REM_DRIFT[$id]}" ""
        fi
        ;;
      min)
        if [[ -z "$pair" ]]; then
          set_check "$id" 'toolchain' tool required failed "$measured" "" "${TOOL_BASE[$id]}" \
            "cannot determine version — ${TOOL_REM_MISS[$id]}" ""
        elif cmp_pair_ge "$pair" "$pin"; then
          set_check "$id" 'toolchain' tool required passed "$measured" "$pair" "${TOOL_BASE[$id]}" "" ""
        else
          set_check "$id" 'toolchain' tool required failed "$measured" "$pair" "${TOOL_BASE[$id]}" "${TOOL_REM_DRIFT[$id]}" ""
        fi
        ;;
    esac
  done
}

eval_docker_compose() {
  # docker compose (Compose v2 plugin). Real mode: needs the docker CLI.
  local present=0 raw_all="" pair="" measured=""
  if [[ -n "$FIXTURE_DIR" ]]; then
    if [[ -r "$FIXTURE_DIR/docker-compose" ]]; then
      present=1; raw_all="$(head -n5 "$FIXTURE_DIR/docker-compose" 2>/dev/null | tr -d '\r')"
    elif [[ -r "$FIXTURE_DIR/docker-compose.absent" ]]; then
      present=0
    else
      if command -v docker >/dev/null 2>&1; then
        raw_all="$(timeout 15 bash -c 'docker compose version' 2>&1 | head -n5 || true)"
        [[ "$raw_all" == *"Compose version"* ]] && present=1 || present=0
      else
        set_check T-DOCKER-COMPOSE 'toolchain' tool required skipped \
          "docker CLI missing" "" "v2.x" "" "depends on T-DOCKER (docker CLI missing)"
        return
      fi
    fi
  else
    if command -v docker >/dev/null 2>&1; then
      raw_all="$(timeout 15 bash -c 'docker compose version' 2>&1 | head -n5 || true)"
      [[ "$raw_all" == *"Compose version"* ]] && present=1 || present=0
    else
      set_check T-DOCKER-COMPOSE 'toolchain' tool required skipped \
        "docker CLI missing" "" "v2.x" "" "depends on T-DOCKER (docker CLI missing)"
      return
    fi
  fi

  pair="$(first_pair "$raw_all")"
  measured="$(printf '%s' "$raw_all" | head -n1)"
  measured="${measured:0:120}"; [[ -z "$measured" ]] && measured="missing"
  if [[ "$present" -eq 0 ]]; then
    set_check T-DOCKER-COMPOSE 'toolchain' tool required failed "$measured" "" "v2 plugin lineage (major >= 2)" \
      "sudo apt-get install -y docker-compose-plugin (Compose v2; legacy docker-compose v1 is not canonical)" ""
  elif [[ -z "$pair" ]]; then
    set_check T-DOCKER-COMPOSE 'toolchain' tool required failed "$measured" "" "v2 plugin lineage (major >= 2)" \
      "cannot determine version — sudo apt-get install -y docker-compose-plugin" ""
  elif (( 10#${pair%%.*} >= 2 )); then
    set_check T-DOCKER-COMPOSE 'toolchain' tool required passed "$measured" "$pair" "v2 plugin lineage (major >= 2)" "" ""
  else
    set_check T-DOCKER-COMPOSE 'toolchain' tool required failed "$measured" "$pair" "v2 plugin lineage (major >= 2)" \
      "restore the Docker Compose v2 plugin lineage (docker compose, major >= 2) — legacy docker-compose v1 is not canonical (docs/64 §4)" ""
  fi
}

eval_docker_daemon() {
  if [[ "$CHECK_DOCKER_DAEMON" -ne 1 ]]; then
    set_check T-DOCKER-DAEMON 'docker daemon' tool advisory skipped \
      "disabled (opt-in)" "" "reachable" "" \
      "disabled by default — probing the Docker daemon needs socket access; enable with --check-docker-daemon"
  elif [[ "$DOCKER_REACHABLE" -eq 1 ]]; then
    set_check T-DOCKER-DAEMON 'docker daemon' tool advisory passed "reachable" "" "reachable" "" ""
  else
    set_check T-DOCKER-DAEMON 'docker daemon' tool advisory warn "unreachable" "" "reachable" \
      "start Docker: sudo systemctl enable --now docker (host privileges — never in a Worker context)" ""
  fi
}

eval_pnpm_pin() {
  # pnpm version must match the packageManager pin in package.json (when it exists).
  if [[ "${CK_STATUS[T-PNPM]:-}" != "passed" ]]; then
    set_check T-PNPM-PIN 'pnpm pin (packageManager)' tool required skipped \
      "pnpm missing" "" "== packageManager field" "" "depends on T-PNPM (pnpm missing)"
    return
  fi
  local pkg="$REPO_PATH/package.json"
  if [[ ! -r "$pkg" ]]; then
    set_check T-PNPM-PIN 'pnpm pin (packageManager)' tool required skipped \
      "package.json absent" "" "== packageManager field" "" "package.json arrives with the T0002 scaffold; pin check deferred"
    return
  fi
  local line pm
  line="$(grep -oE '"packageManager"[[:space:]]*:[[:space:]]*"[^"]*"' "$pkg" 2>/dev/null | head -n1)"
  pm="$(printf '%s' "$line" | sed -nE 's/.*"([^"]+)"[[:space:]]*$/\1/p')"
  if [[ -z "$pm" || "$pm" != pnpm@* ]]; then
    set_check T-PNPM-PIN 'pnpm pin (packageManager)' tool required skipped \
      "field missing or malformed" "" "== packageManager field" "" \
      "packageManager field is missing or not a pnpm pin in $pkg"
    return
  fi
  local pinver="${pm#pnpm@}"
  local pnpm_measured=""
  pnpm_measured="$(printf '%s' "${CK_MEASURED[T-PNPM]:-}" | head -n1)"
  if [[ "$pnpm_measured" == "$pinver" ]]; then
    set_check T-PNPM-PIN 'pnpm pin (packageManager)' tool required passed \
      "pnpm $pnpm_measured == pin $pinver" "" "== packageManager field" "" ""
  else
    set_check T-PNPM-PIN 'pnpm pin (packageManager)' tool required failed \
      "pnpm $pnpm_measured != pin $pinver" "" "== packageManager field" \
      "align pnpm with the pin: corepack enable && corepack prepare pnpm@$pinver --activate — pin drift requires Supervisor sign-off" ""
  fi
}

compute_verdict() {
  local id req_failed=0 env_failed=0
  for id in "${ENV_IDS[@]}" "${RES_IDS[@]}" "${TOOL_IDS[@]}"; do
    if [[ "${CK_SEVERITY[$id]}" == "required" && "${CK_STATUS[$id]}" == "failed" ]]; then
      req_failed=1
      [[ "${CK_CATEGORY[$id]}" == "env" ]] && env_failed=1
    fi
  done
  if [[ "$env_failed" -eq 1 ]]; then
    VERDICT="unsupported"; EXIT_CODE=2
  elif [[ "$req_failed" -eq 1 ]]; then
    VERDICT="toolchain_failure"; EXIT_CODE=1
  else
    VERDICT="ok"; EXIT_CODE=0
  fi
}

# ------------------------------------------------------------------- output
emit_human() {
  local id tag status sev rem reason
  printf 'POST environment preflight (%s) — contract %s\n' "$SCRIPT_NAME" "$CONTRACT_REF"
  printf 'Environment: kernel=%s arch=%s distro=%s %s (%s) wsl=%s\n' \
    "${KERNEL:-unknown}" "${ARCH:-unknown}" "${DISTRO_ID:-unknown}" \
    "${DISTRO_VERSION_ID:-unknown}" "${CODENAME:-unknown}" "$WSL_KIND"
  printf 'Repo: %s (fs mount: %s)\n\n' "$REPO_PATH" "${REPO_FS_MOUNT:-unknown}"

  for id in "${ENV_IDS[@]}" "${RES_IDS[@]}" "${TOOL_IDS[@]}"; do
    status="${CK_STATUS[$id]}"; sev="${CK_SEVERITY[$id]}"
    rem="${CK_REMEDIATION[$id]}"; reason="${CK_REASON[$id]}"
    case "$status" in
      passed) tag="PASS" ;; failed) tag="FAIL" ;; warn) tag="WARN" ;; skipped) tag="SKIP" ;;
    esac
    printf '[%s] %-17s %s — baseline: %s\n' "$tag" "$id" "${CK_MEASURED[$id]}" "${CK_BASELINE[$id]}"
    if [[ "$status" == "failed" || "$status" == "warn" ]]; then
      printf '       fix: %s\n' "$rem"
    elif [[ "$status" == "skipped" ]]; then
      printf '       note: %s\n' "$reason"
    fi
  done

  local rt rp rf rs at ap aw as
  rt=0; rp=0; rf=0; rs=0; at=0; ap=0; aw=0; as=0
  for id in "${ENV_IDS[@]}" "${RES_IDS[@]}" "${TOOL_IDS[@]}"; do
    if [[ "${CK_SEVERITY[$id]}" == "required" ]]; then
      rt=$((rt+1))
      case "${CK_STATUS[$id]}" in
        passed) rp=$((rp+1)) ;; failed) rf=$((rf+1)) ;; skipped) rs=$((rs+1)) ;;
      esac
    else
      at=$((at+1))
      case "${CK_STATUS[$id]}" in
        passed) ap=$((ap+1)) ;; warn) aw=$((aw+1)) ;; skipped) as=$((as+1)) ;;
      esac
    fi
  done
  printf '\nSummary: required %d checks: %d passed, %d failed, %d skipped | advisory %d checks: %d passed, %d warn, %d skipped\n' \
    "$rt" "$rp" "$rf" "$rs" "$at" "$ap" "$aw" "$as"
  printf 'Verdict: %s (exit %d)\n' "$VERDICT" "$EXIT_CODE"
}

emit_json() {
  local id out="" name cat sev status meas ver base rem reason first=1
  out='{'
  out+='"schema_version":'"$SCHEMA_VERSION"','
  out+='"contract":"ops/doctor-checks.md",'
  out+='"verdict":"'"$VERDICT"'",'
  out+='"exit_code":'"$EXIT_CODE"','
  out+='"environment":{"kernel":"'"$(json_escape "${KERNEL:-}")"'"'
  out+=',"arch":"'"$(json_escape "${ARCH:-}")"'"'
  out+=',"distro_id":"'"$(json_escape "${DISTRO_ID:-}")"'"'
  out+=',"distro_version_id":"'"$(json_escape "${DISTRO_VERSION_ID:-}")"'"'
  out+=',"distro_pretty":"'"$(json_escape "${DISTRO_PRETTY:-}")"'"'
  out+=',"codename":"'"$(json_escape "${CODENAME:-}")"'"'
  out+=',"wsl":"'"$(json_escape "$WSL_KIND")"'"'
  out+=',"repo_path":"'"$(json_escape "$REPO_PATH")"'"'
  out+=',"repo_fs_mount":"'"$(json_escape "${REPO_FS_MOUNT:-}")"'"'
  out+='},'
  out+='"baselines":{'
  out+='"go":"1.27.x","node":"24.x","python3":">= 3.12","psql":"16.x","redis-cli":"7.x",'
  out+='"git":"2.x","git-lfs":"3.x","jq":"1.x","make":"4.x","shellcheck":"recorded",'
  out+='"docker-compose":"v2 plugin lineage (>= 2)","claude":"recorded","uv":"recorded","pnpm":"packageManager pin",'
  out+='"cpu":">= 8","ram":">= 32 GiB","swap":">= 8 GiB","disk":">= 20 GiB",'
  out+='"fd":">= 65535","docker-disk":">= 100 GiB"'
  out+='},'
  out+='"checks":['
  for id in "${ENV_IDS[@]}" "${RES_IDS[@]}" "${TOOL_IDS[@]}"; do
    name="${CK_NAME[$id]}"; cat="${CK_CATEGORY[$id]}"; sev="${CK_SEVERITY[$id]}"
    status="${CK_STATUS[$id]}"; meas="${CK_MEASURED[$id]}"; ver="${CK_VERSION[$id]}"
    base="${CK_BASELINE[$id]}"; rem="${CK_REMEDIATION[$id]}"; reason="${CK_REASON[$id]}"
    [[ "$first" -eq 1 ]] && first=0 || out+=','
    out+='{"id":"'"$id"'","name":"'"$(json_escape "$name")"'","category":"'"$cat"'",'
    out+='"severity":"'"$sev"'","status":"'"$status"'",'
    out+='"measured":"'"$(json_escape "$meas")"'",'
    if [[ -n "$ver" ]]; then out+='"version":"'"$(json_escape "$ver")"'",'; else out+='"version":null,'; fi
    out+='"baseline":"'"$(json_escape "$base")"'",'
    if [[ -n "$rem" ]]; then out+='"remediation":"'"$(json_escape "$rem")"'",'; else out+='"remediation":null,'; fi
    if [[ -n "$reason" ]]; then out+='"reason":"'"$(json_escape "$reason")"'"'; else out+='"reason":null'; fi
    out+='}'
  done
  out+='],'
  local rt rp rf rs at ap aw as
  rt=0; rp=0; rf=0; rs=0; at=0; ap=0; aw=0; as=0
  for id in "${ENV_IDS[@]}" "${RES_IDS[@]}" "${TOOL_IDS[@]}"; do
    if [[ "${CK_SEVERITY[$id]}" == "required" ]]; then
      rt=$((rt+1))
      case "${CK_STATUS[$id]}" in
        passed) rp=$((rp+1)) ;; failed) rf=$((rf+1)) ;; skipped) rs=$((rs+1)) ;;
      esac
    else
      at=$((at+1))
      case "${CK_STATUS[$id]}" in
        passed) ap=$((ap+1)) ;; warn) aw=$((aw+1)) ;; skipped) as=$((as+1)) ;;
      esac
    fi
  done
  out+='"summary":{"required":{"total":'"$rt"',"passed":'"$rp"',"failed":'"$rf"',"skipped":'"$rs"'},'
  out+='"advisory":{"total":'"$at"',"passed":'"$ap"',"warn":'"$aw"',"skipped":'"$as"'}}'
  out+='}'
  printf '%s\n' "$out"
}

# ------------------------------------------------------------------- main
VERDICT=""; EXIT_CODE=0

main() {
  parse_args "$@"
  init_checks
  measure_env
  measure_resources
  if [[ "$CHECK_DOCKER_DAEMON" -eq 1 ]]; then
    measure_docker_daemon
  fi
  eval_env
  eval_resources
  eval_tools
  compute_verdict
  if [[ "$JSON_MODE" -eq 1 ]]; then
    emit_json
  else
    emit_human
  fi
  exit "$EXIT_CODE"
}

main "$@"
