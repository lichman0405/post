#!/usr/bin/env bash
#
# Unit tests for ops/doctor.sh decision logic (fixture-driven).
# Runnable on a host with bash + coreutils + python3 only.
#
# Fixture mode (--fixture) short-circuits all real version commands, so these
# tests never touch the host toolchain and never invoke docker. Missing-tool
# detection and remediation text are exercised via fixture files
# (<tool>.absent), not by deleting real tools. See ops/doctor-checks.md §6.
set -u
export LC_ALL=C

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
DOCTOR="$ROOT/ops/doctor.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

FAILS=0
CASES=0
RC=0; OUT=""; ERR=""

fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

assert_eq() { # desc expected actual
  if [[ "$2" == "$3" ]]; then ok "$1"; else fail "$1: expected [$2] got [$3]"; fi
}
assert_contains() { # desc file needle
  if grep -qF -- "$3" "$2"; then ok "$1"; else fail "$1: [$3] not found in $2"; fi
}
assert_absent() { # desc file needle
  if grep -qF -- "$3" "$2"; then fail "$1: [$3] unexpectedly found in $2"; else ok "$1"; fi
}

jget() { # file key... -> value
  python3 - "$@" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
for k in sys.argv[2:]:
    d = d[int(k)] if isinstance(d, list) else d[k]
print(d)
PY
}

find_check() { # file id field -> value
  python3 - "$@" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
c = [x for x in d["checks"] if x["id"] == sys.argv[2]][0]
v = c.get(sys.argv[3], "")
print("" if v is None else v)
PY
}

# Canonical all-green fixture: Ubuntu 24.04 amd64, generous resources, every
# tool at its pinned baseline, packageManager pin matched, docker daemon up.
mkfixture() { # dir
  local d="$1"
  mkdir -p "$d/repo"
  printf 'Linux\n'                > "$d/os.kernel"
  printf 'x86_64\n'               > "$d/os.arch"
  printf 'ubuntu\n'               > "$d/os.id"
  printf '24.04\n'                > "$d/os.version_id"
  printf 'Ubuntu 24.04.5 LTS\n'   > "$d/os.pretty"
  printf 'noble\n'                > "$d/os.codename"
  printf 'none\n'                 > "$d/os.wsl"
  printf '16\n'                   > "$d/res.nproc"
  printf '73400320\n'             > "$d/res.mem_total_kb"          # 70 GiB
  printf '67108864\n'             > "$d/res.swap_total_kb"         # 64 GiB
  printf '1048576\n'              > "$d/res.fd_limit"
  printf '209715200\n'            > "$d/res.disk_free_kb"          # 200 GiB
  printf 'true\n'                 > "$d/res.docker_reachable"
  printf '131072000\n'            > "$d/res.docker_root_free_kb"   # 125 GiB
  printf 'present\n'              > "$d/res.bwrap"
  printf 'present\n'              > "$d/res.socat"
  printf 'git version 2.43.0\n'                          > "$d/git"
  printf 'git-lfs/3.4.1 (GitHub; linux amd64)\n'         > "$d/git-lfs"
  printf '2.1.269 (Claude Code)\n'                        > "$d/claude"
  printf 'go version go1.27.1 linux/amd64\n'              > "$d/go"
  printf 'v24.21.0\n'                                     > "$d/node"
  printf '12.4.1\n'                                       > "$d/pnpm"
  printf 'Python 3.12.7\n'                                > "$d/python3"
  printf 'uv 0.7.9\n'                                     > "$d/uv"
  printf 'Docker version 29.8.0, build abc123\n'          > "$d/docker"
  printf 'Docker Compose version v2.39.4\n'               > "$d/docker-compose"
  printf 'jq-1.6\n'                                       > "$d/jq"
  printf 'psql (PostgreSQL) 16.15\n'                      > "$d/psql"
  printf 'redis-cli 7.0.15\n'                             > "$d/redis-cli"
  printf 'GNU Make 4.3\n'                                 > "$d/make"
  printf 'ripgrep 14.1.1\n'                               > "$d/rg"
  printf 'ShellCheck - shell script analysis tool\nversion: 0.10.0\n' > "$d/shellcheck"
  printf '{"name":"post","packageManager":"pnpm@12.4.1"}\n' > "$d/repo/package.json"
  printf '%s/repo\n' "$d"                                  > "$d/os.repo_path"
}

# repoint DIR: fix os.repo_path after copying the template fixture to a new dir
repoint() { printf '%s/repo\n' "$1" > "$1/os.repo_path"; }

# run_json NAME FIXTURE_DIR [doctor flags...] -> sets RC, OUT, ERR
run_json() {
  local name="$1" dir="$2"; shift 2
  CASES=$((CASES+1))
  OUT="$WORK/$name.json"; ERR="$WORK/$name.err"
  "$DOCTOR" --fixture "$dir" "$@" --json >"$OUT" 2>"$ERR"
  RC=$?
  if ! python3 -c "import json,sys; json.load(open(sys.argv[1]))" "$OUT" 2>/dev/null; then
    fail "$name: stdout is not valid JSON (rc=$RC)"
    cat "$OUT"; return
  fi
}

FIX="$WORK/fix"; mkfixture "$FIX"

# ---------------------------------------------------------------- case 1: canonical all-green
run_json canonical "$FIX" --check-docker-daemon
assert_eq "canonical: exit code" "0" "$RC"
assert_eq "canonical: verdict" "ok" "$(jget "$OUT" verdict)"
assert_eq "canonical: exit_code field" "0" "$(jget "$OUT" exit_code)"
assert_eq "canonical: T-GO status" "passed" "$(find_check "$OUT" T-GO status)"
assert_eq "canonical: T-GO version" "1.27" "$(find_check "$OUT" T-GO version)"
assert_eq "canonical: T-PNPM-PIN status" "passed" "$(find_check "$OUT" T-PNPM-PIN status)"
assert_eq "canonical: RES-DOCKER-DISK status" "passed" "$(find_check "$OUT" RES-DOCKER-DISK status)"
assert_eq "canonical: T-DOCKER-DAEMON status" "passed" "$(find_check "$OUT" T-DOCKER-DAEMON status)"
assert_eq "canonical: RES-REPO-FS status" "skipped" "$(find_check "$OUT" RES-REPO-FS status)"
assert_eq "canonical: required.failed" "0" "$(jget "$OUT" summary required failed)"
assert_eq "canonical: advisory.warn" "0" "$(jget "$OUT" summary advisory warn)"
"$DOCTOR" --fixture "$FIX" --check-docker-daemon >"$WORK/canonical.human" 2>/dev/null; HRC=$?
assert_eq "canonical: human exit code" "0" "$HRC"
assert_contains "canonical: human verdict line" "$WORK/canonical.human" "Verdict: ok (exit 0)"
assert_absent  "canonical: no FAIL lines" "$WORK/canonical.human" "[FAIL]"

# ---------------------------------------------------------------- case 2: missing required tool -> named + remediation
D2="$WORK/c2"; cp -r "$FIX" "$D2"; repoint "$D2"; rm -f "$D2/go"; touch "$D2/go.absent"
run_json go-missing "$D2"
assert_eq "go-missing: exit code" "1" "$RC"
assert_eq "go-missing: verdict" "toolchain_failure" "$(jget "$OUT" verdict)"
assert_eq "go-missing: T-GO status" "failed" "$(find_check "$OUT" T-GO status)"
assert_contains "go-missing: names go in remediation" "$OUT" "Go 1.27.x"
assert_contains "go-missing: measured is missing" "$OUT" "missing"
"$DOCTOR" --fixture "$D2" >"$WORK/go-missing.human" 2>/dev/null; HRC=$?
assert_eq "go-missing: human exit code" "1" "$HRC"
assert_contains "go-missing: human names T-GO" "$WORK/go-missing.human" "T-GO"
assert_contains "go-missing: human has fix line" "$WORK/go-missing.human" "fix:"
assert_contains "go-missing: human remediation names Go" "$WORK/go-missing.human" "Go 1.27.x"
assert_contains "go-missing: human verdict" "$WORK/go-missing.human" "Verdict: toolchain_failure (exit 1)"

# ---------------------------------------------------------------- case 3: major-version drift is reported, not auto-fixed
D3="$WORK/c3"; cp -r "$FIX" "$D3"; repoint "$D3"; printf 'go version go1.28.0 linux/amd64\n' > "$D3/go"
run_json go-drift "$D3"
assert_eq "go-drift: exit code" "1" "$RC"
assert_eq "go-drift: T-GO status" "failed" "$(find_check "$OUT" T-GO status)"
assert_eq "go-drift: T-GO version parsed" "1.28" "$(find_check "$OUT" T-GO version)"
assert_contains "go-drift: remediation names pinned baseline" "$OUT" "1.27.x"
assert_contains "go-drift: remediation mentions Supervisor sign-off" "$OUT" "Supervisor"

# ---------------------------------------------------------------- case 4: version below baseline (python 3.11)
D4="$WORK/c4"; cp -r "$FIX" "$D4"; repoint "$D4"; printf 'Python 3.11.9\n' > "$D4/python3"
run_json python-below "$D4"
assert_eq "python-below: exit code" "1" "$RC"
assert_eq "python-below: T-PYTHON3 status" "failed" "$(find_check "$OUT" T-PYTHON3 status)"
assert_contains "python-below: remediation names 3.12" "$OUT" "3.12"

# ---------------------------------------------------------------- case 5: Windows Native -> unsupported + WSL2 pointer
D5="$WORK/c5"; cp -r "$FIX" "$D5"; repoint "$D5"; printf 'MINGW64_NT-10.0-22631\n' > "$D5/os.kernel"
run_json windows-native "$D5"
assert_eq "windows-native: exit code" "2" "$RC"
assert_eq "windows-native: verdict" "unsupported" "$(jget "$OUT" verdict)"
assert_eq "windows-native: ENV-OS-KERNEL status" "failed" "$(find_check "$OUT" ENV-OS-KERNEL status)"
assert_contains "windows-native: remediation names WSL2" "$OUT" "WSL2"
assert_contains "windows-native: remediation names Linux filesystem" "$OUT" "Linux filesystem"
"$DOCTOR" --fixture "$D5" >"$WORK/windows-native.human" 2>/dev/null; HRC=$?
assert_eq "windows-native: human exit code" "2" "$HRC"
assert_contains "windows-native: human says Windows Native" "$WORK/windows-native.human" "Windows Native"
assert_contains "windows-native: human says WSL2" "$WORK/windows-native.human" "WSL2"

# ---------------------------------------------------------------- case 6: other non-Linux kernel -> unsupported
D6="$WORK/c6"; cp -r "$FIX" "$D6"; repoint "$D6"; printf 'Darwin\n' > "$D6/os.kernel"
run_json darwin "$D6"
assert_eq "darwin: exit code" "2" "$RC"
assert_eq "darwin: verdict" "unsupported" "$(jget "$OUT" verdict)"

# ---------------------------------------------------------------- case 6b: non-Ubuntu Linux distro -> unsupported
D6B="$WORK/c6b"; cp -r "$FIX" "$D6B"; repoint "$D6B"
printf 'debian\n' > "$D6B/os.id"
printf '12\n' > "$D6B/os.version_id"
run_json debian "$D6B"
assert_eq "debian: exit code" "2" "$RC"
assert_eq "debian: verdict" "unsupported" "$(jget "$OUT" verdict)"
assert_eq "debian: ENV-OS-DISTRO status" "failed" "$(find_check "$OUT" ENV-OS-DISTRO status)"

# ---------------------------------------------------------------- case 7: WSL1 -> advisory warn, exit 0
D7="$WORK/c7"; cp -r "$FIX" "$D7"; repoint "$D7"; printf 'wsl1\n' > "$D7/os.wsl"
run_json wsl1 "$D7"
assert_eq "wsl1: exit code" "0" "$RC"
assert_eq "wsl1: ENV-WSL status" "warn" "$(find_check "$OUT" ENV-WSL status)"
assert_eq "wsl1: advisory.warn" "1" "$(jget "$OUT" summary advisory warn)"

# ---------------------------------------------------------------- case 8: WSL2 with repo under /mnt -> required failure
D8="$WORK/c8"; cp -r "$FIX" "$D8"; repoint "$D8"
printf 'wsl2\n' > "$D8/os.wsl"
printf '/mnt/c/Users/dev/post\n' > "$D8/os.repo_path"
run_json wsl2-repo-on-mnt "$D8"
assert_eq "wsl2-repo-on-mnt: exit code" "1" "$RC"
assert_eq "wsl2-repo-on-mnt: RES-REPO-FS status" "failed" "$(find_check "$OUT" RES-REPO-FS status)"
assert_contains "wsl2-repo-on-mnt: remediation names /mnt" "$OUT" "/mnt"

# ---------------------------------------------------------------- case 9: pnpm pin mismatch -> failed
D9="$WORK/c9"; cp -r "$FIX" "$D9"; repoint "$D9"; printf '12.0.0\n' > "$D9/pnpm"
run_json pnpm-pin-mismatch "$D9"
assert_eq "pnpm-pin-mismatch: exit code" "1" "$RC"
assert_eq "pnpm-pin-mismatch: T-PNPM-PIN status" "failed" "$(find_check "$OUT" T-PNPM-PIN status)"
assert_contains "pnpm-pin-mismatch: remediation names pin" "$OUT" "12.4.1"

# ---------------------------------------------------------------- case 10: malformed packageManager -> skipped
D10="$WORK/c10"; cp -r "$FIX" "$D10"; repoint "$D10"
printf '{"name":"post","packageManager":"npm@10.0.0"}\n' > "$D10/repo/package.json"
run_json pnpm-pin-malformed "$D10"
assert_eq "pnpm-pin-malformed: exit code" "0" "$RC"
assert_eq "pnpm-pin-malformed: T-PNPM-PIN status" "skipped" "$(find_check "$OUT" T-PNPM-PIN status)"

# ---------------------------------------------------------------- case 11: docker daemon unreachable -> warn + skip, exit 0
D11="$WORK/c11"; cp -r "$FIX" "$D11"; repoint "$D11"; printf 'false\n' > "$D11/res.docker_reachable"
run_json docker-daemon-down "$D11" --check-docker-daemon
assert_eq "docker-daemon-down: exit code" "0" "$RC"
assert_eq "docker-daemon-down: T-DOCKER-DAEMON status" "warn" "$(find_check "$OUT" T-DOCKER-DAEMON status)"
assert_eq "docker-daemon-down: RES-DOCKER-DISK status" "skipped" "$(find_check "$OUT" RES-DOCKER-DISK status)"

# ---------------------------------------------------------------- case 12: docker compose plugin missing -> failed with remediation
D12="$WORK/c12"; cp -r "$FIX" "$D12"; repoint "$D12"; rm -f "$D12/docker-compose"; touch "$D12/docker-compose.absent"
run_json compose-missing "$D12"
assert_eq "compose-missing: exit code" "1" "$RC"
assert_eq "compose-missing: T-DOCKER-COMPOSE status" "failed" "$(find_check "$OUT" T-DOCKER-COMPOSE status)"
assert_contains "compose-missing: remediation names plugin" "$OUT" "docker-compose-plugin"

# ---------------------------------------------------------------- case 12b: Compose v5 (v2 plugin lineage) passes
D12B="$WORK/c12b"; cp -r "$FIX" "$D12B"; repoint "$D12B"
printf 'Docker Compose version v5.5.1\n' > "$D12B/docker-compose"
run_json compose-v5 "$D12B"
assert_eq "compose-v5: exit code" "0" "$RC"
assert_eq "compose-v5: T-DOCKER-COMPOSE status" "passed" "$(find_check "$OUT" T-DOCKER-COMPOSE status)"
assert_eq "compose-v5: T-DOCKER-COMPOSE version recorded" "5.5" "$(find_check "$OUT" T-DOCKER-COMPOSE version)"

# ---------------------------------------------------------------- case 12c: Compose major < 2 (legacy v1 semantics) fails
D12C="$WORK/c12c"; cp -r "$FIX" "$D12C"; repoint "$D12C"
printf 'Docker Compose version v1.99.9\n' > "$D12C/docker-compose"
run_json compose-v1 "$D12C"
assert_eq "compose-v1: exit code" "1" "$RC"
assert_eq "compose-v1: T-DOCKER-COMPOSE status" "failed" "$(find_check "$OUT" T-DOCKER-COMPOSE status)"
assert_contains "compose-v1: remediation names v2 plugin lineage" "$OUT" "v2 plugin lineage"

# ---------------------------------------------------------------- case 13: default run never probes docker daemon
run_json default-no-daemon "$FIX"
assert_eq "default-no-daemon: exit code" "0" "$RC"
assert_eq "default-no-daemon: RES-DOCKER-DISK status" "skipped" "$(find_check "$OUT" RES-DOCKER-DISK status)"
assert_eq "default-no-daemon: T-DOCKER-DAEMON status" "skipped" "$(find_check "$OUT" T-DOCKER-DAEMON status)"
assert_contains "default-no-daemon: reason mentions opt-in flag" "$OUT" "--check-docker-daemon"

# ---------------------------------------------------------------- case 14: usage errors -> exit 3, no JSON
CASES=$((CASES+1))
"$DOCTOR" --bogus >"$WORK/usage.out" 2>"$WORK/usage.err"; URC=$?
assert_eq "usage: unknown option exit code" "3" "$URC"
assert_eq "usage: no stdout on usage error" "" "$(cat "$WORK/usage.out")"
assert_contains "usage: stderr names the option" "$WORK/usage.err" "unknown option"
CASES=$((CASES+1))
"$DOCTOR" --fixture /nonexistent-dir-xyz >"$WORK/usage2.out" 2>"$WORK/usage2.err"; URC=$?
assert_eq "usage: bad fixture dir exit code" "3" "$URC"
assert_contains "usage: bad fixture dir message" "$WORK/usage2.err" "fixture directory not found"

# ---------------------------------------------------------------- case 15: determinism (byte-identical outputs)
"$DOCTOR" --fixture "$FIX" --check-docker-daemon --json >"$WORK/det1.json" 2>/dev/null
"$DOCTOR" --fixture "$FIX" --check-docker-daemon --json >"$WORK/det2.json" 2>/dev/null
if cmp -s "$WORK/det1.json" "$WORK/det2.json"; then ok "determinism: JSON byte-identical"; else fail "determinism: JSON outputs differ"; fi
"$DOCTOR" --fixture "$FIX" --check-docker-daemon >"$WORK/det1.txt" 2>/dev/null
"$DOCTOR" --fixture "$FIX" --check-docker-daemon >"$WORK/det2.txt" 2>/dev/null
if cmp -s "$WORK/det1.txt" "$WORK/det2.txt"; then ok "determinism: human output byte-identical"; else fail "determinism: human outputs differ"; fi

# ---------------------------------------------------------------- case 16: ripgrep binary name note in remediation
D16="$WORK/c16"; cp -r "$FIX" "$D16"; repoint "$D16"; rm -f "$D16/rg"; touch "$D16/rg.absent"
run_json rg-missing "$D16"
assert_eq "rg-missing: exit code" "1" "$RC"
assert_eq "rg-missing: T-RG status" "failed" "$(find_check "$OUT" T-RG status)"
assert_contains "rg-missing: remediation notes binary name" "$OUT" "rg"

# ---------------------------------------------------------------- summary
printf '\nunit: %d cases, %d assertion failures\n' "$CASES" "$FAILS"
if [[ "$FAILS" -gt 0 ]]; then
  printf 'UNIT TEST RESULT: FAIL\n'
  exit 1
fi
printf 'UNIT TEST RESULT: PASS\n'
exit 0
