#!/usr/bin/env bash
#
# End-to-end smoke test for ops/doctor.sh: runs the REAL preflight on this host
# and verifies that human output, JSON output, exit codes and summary counts are
# produced and self-consistent. The verdict itself is not required to be "ok" —
# the host is judged by the preflight; this test judges the preflight.
#
# Runnable on a host with bash + coreutils + python3 only.
# Note: the preflight performs docker CLI *version* queries (no socket access);
# the docker daemon probe stays off unless --check-docker-daemon is passed.
set -u
export LC_ALL=C

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
DOCTOR="$ROOT/ops/doctor.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

FAILS=0
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

cd "$ROOT" || exit 1

# 1. syntax check
if bash -n "$DOCTOR"; then ok "bash -n ops/doctor.sh"; else fail "bash -n ops/doctor.sh"; fi

# 2. real runs: human once, JSON twice (determinism)
"$DOCTOR" >"$WORK/human.txt" 2>"$WORK/human.err"; HRC=$?
"$DOCTOR" --json >"$WORK/out1.json" 2>"$WORK/err1"; JRC1=$?
"$DOCTOR" --json >"$WORK/out2.json" 2>"$WORK/err2"; JRC2=$?

if [[ -s "$WORK/human.txt" ]]; then ok "human output produced"; else fail "human output empty"; fi
if [[ ! -s "$WORK/human.err" ]]; then ok "human mode stderr empty"; else fail "human mode stderr not empty"; fi
if [[ ! -s "$WORK/err1" ]]; then ok "json mode stderr empty"; else fail "json mode stderr not empty"; fi

python3 - "$WORK" "$ROOT" "$HRC" "$JRC1" "$JRC2" <<'PY'
import json, sys
w, root, hrc, jrc1, jrc2 = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4]), int(sys.argv[5])
fails = []
def ok(msg): print("ok   " + msg)
def bad(msg):
    fails.append(msg); print("FAIL " + msg)

try:
    d1 = json.load(open(w + "/out1.json"))
    d2 = json.load(open(w + "/out2.json"))
except Exception as e:
    bad("JSON parse: %s" % e); raise SystemExit(1)

if d1 == d2:
    ok("deterministic: two --json runs are byte-identical (parsed equality)")
else:
    bad("deterministic: two --json runs differ")

human = open(w + "/human.txt").read()

# expected catalog: contract ops/doctor-checks.md §7, fixed order
EXPECTED = [
    "ENV-OS-KERNEL","ENV-OS-DISTRO","ENV-OS-ARCH","ENV-WSL",
    "RES-CPU","RES-CPU-TIER","RES-RAM","RES-RAM-TIER","RES-SWAP","RES-SWAP-TIER",
    "RES-DISK","RES-DISK-TIER","RES-FD","RES-DOCKER-DISK","RES-REPO-FS","RES-BWRAP","RES-SOCAT",
    "T-GIT","T-GIT-LFS","T-CLAUDE","T-GO","T-NODE","T-PNPM","T-PNPM-PIN","T-PYTHON3","T-UV",
    "T-DOCKER","T-DOCKER-COMPOSE","T-DOCKER-DAEMON","T-JQ","T-PSQL","T-REDIS-CLI","T-MAKE","T-RG","T-SHELLCHECK",
]
ids = [c["id"] for c in d1["checks"]]
if ids == EXPECTED:
    ok("check catalog: all %d ids in contract order" % len(EXPECTED))
else:
    bad("check catalog mismatch: %s" % ids)

# exit-code consistency
if d1["exit_code"] == jrc1 == jrc2:
    ok("exit_code field == real exit code (%d)" % jrc1)
else:
    bad("exit codes inconsistent: field=%s human_rc=%s json_rc1=%s json_rc2=%s" % (d1["exit_code"], hrc, jrc1, jrc2))
if hrc == jrc1:
    ok("human and json runs agree on exit code")
else:
    bad("human rc %d != json rc %d" % (hrc, jrc1))

# verdict mapping
mapping = {"ok": 0, "toolchain_failure": 1, "unsupported": 2}
if mapping.get(d1["verdict"]) == d1["exit_code"]:
    ok("verdict %r maps to exit %d" % (d1["verdict"], d1["exit_code"]))
else:
    bad("verdict/exit-code mapping broken: %r/%d" % (d1["verdict"], d1["exit_code"]))

env = d1["environment"]
if env["kernel"] == "Linux" and env["arch"] == "x86_64" and env["distro_id"] == "ubuntu" and env["distro_version_id"] == "24.04":
    ok("host identified as Ubuntu 24.04 amd64 Linux")
    if d1["verdict"] != "unsupported":
        ok("verdict is %r (not unsupported, as expected on this host)" % d1["verdict"])
    else:
        bad("verdict is 'unsupported' although env checks read canonical")
else:
    bad("unexpected environment: %s" % env)

# summary == checks accounting
req = {"total": 0, "passed": 0, "failed": 0, "skipped": 0}
adv = {"total": 0, "passed": 0, "warn": 0, "skipped": 0}
for c in d1["checks"]:
    s, sev, st = c["id"], c["severity"], c["status"]
    if sev == "required":
        req["total"] += 1; req[st] = req.get(st, 0) + 1
    else:
        adv["total"] += 1; adv[st] = adv.get(st, 0) + 1
if d1["summary"]["required"] == req and d1["summary"]["advisory"] == adv:
    ok("summary counts match checks: required=%s advisory=%s" % (req, adv))
else:
    bad("summary mismatch: json=%s computed=%s" % (d1["summary"], {"required": req, "advisory": adv}))

# schema conformance (inline subset of ops/doctor-output.schema.json; no external libs)
schema = json.load(open(root + "/ops/doctor-output.schema.json"))
if set(d1.keys()) == set(schema["required"]):
    ok("schema: top-level keys match")
else:
    bad("schema: top-level keys mismatch: %s" % set(d1.keys()))
if d1["schema_version"] == schema["properties"]["schema_version"]["const"] and \
   d1["contract"] == schema["properties"]["contract"]["const"]:
    ok("schema: schema_version/contract consts hold")
else:
    bad("schema: consts broken")
if d1["verdict"] in schema["properties"]["verdict"]["enum"]:
    ok("schema: verdict enum")
else:
    bad("schema: verdict enum")
env_s = schema["properties"]["environment"]
if set(d1["environment"].keys()) == set(env_s["required"]) and \
   d1["environment"]["wsl"] in env_s["properties"]["wsl"]["enum"]:
    ok("schema: environment shape")
else:
    bad("schema: environment shape")
bas_s = schema["properties"]["baselines"]
if set(d1["baselines"].keys()) == set(bas_s["required"]) and \
   all(d1["baselines"][k] == bas_s["properties"][k]["const"] for k in bas_s["required"]):
    ok("schema: baselines keys + consts hold")
else:
    bad("schema: baselines mismatch: %s" % d1["baselines"])
import re
chk_s = schema["properties"]["checks"]["items"]
schk = chk_s["required"]
for c in d1["checks"]:
    if set(c.keys()) != set(schk):
        bad("schema: check %s keys %s" % (c["id"], set(c.keys())))
    if not re.match(chk_s["properties"]["id"]["pattern"], c["id"]):
        bad("schema: check %s id pattern" % c["id"])
    if c["category"] not in chk_s["properties"]["category"]["enum"]:
        bad("schema: check %s category" % c["id"])
    if c["severity"] not in chk_s["properties"]["severity"]["enum"]:
        bad("schema: check %s severity" % c["id"])
    if c["status"] not in chk_s["properties"]["status"]["enum"]:
        bad("schema: check %s status" % c["id"])
    for f in ("version", "remediation", "reason"):
        if c[f] is not None and not isinstance(c[f], str):
            bad("schema: check %s %s type" % (c["id"], f))

# per-check invariants
problems = []
for c in d1["checks"]:
    if not c["baseline"]:
        problems.append(c["id"] + ": empty baseline")
    if not c["measured"]:
        problems.append(c["id"] + ": empty measured")
    if c["status"] in ("failed", "warn") and not c["remediation"]:
        problems.append(c["id"] + ": %s without remediation" % c["status"])
    if c["status"] == "skipped" and not c["reason"]:
        problems.append(c["id"] + ": skipped without reason")
    if c["status"] == "failed" and c["severity"] == "advisory":
        problems.append(c["id"] + ": advisory check must never be failed")
    if c["status"] == "warn" and c["severity"] == "required":
        problems.append(c["id"] + ": required check must never be warn")
if problems:
    for p in problems: bad("check invariant: " + p)
else:
    ok("per-check invariants hold (remediation/reason/severity rules)")

# human/JSON consistency
cnt = lambda t: sum(1 for l in human.splitlines() if l.startswith("[" + t + "]"))
pf, rf, wf, sf = cnt("PASS"), cnt("FAIL"), cnt("WARN"), cnt("SKIP")
exp_pass = req["passed"] + adv["passed"]
exp_fail = req["failed"]
exp_warn = adv["warn"]
exp_skip = req["skipped"] + adv["skipped"]
if (pf, rf, wf, sf) == (exp_pass, exp_fail, exp_warn, exp_skip):
    ok("human output tags match JSON: PASS=%d FAIL=%d WARN=%d SKIP=%d" % (pf, rf, wf, sf))
else:
    bad("human tags mismatch: got PASS=%d FAIL=%d WARN=%d SKIP=%d, expected %d %d %d %d" % (pf, rf, wf, sf, exp_pass, exp_fail, exp_warn, exp_skip))
if ("Verdict: %s (exit %d)" % (d1["verdict"], d1["exit_code"])) in human:
    ok("human verdict line matches JSON verdict")
else:
    bad("human verdict line missing/mismatched")

print("SMOKE VERDICT: %s (exit %d) | required %d/%d passed, %d failed, %d skipped" % (
    d1["verdict"], d1["exit_code"], req["passed"], req["total"], req["failed"], req["skipped"]))
for c in d1["checks"]:
    if c["status"] in ("failed", "warn"):
        print("  [%s] %s: %s — baseline: %s" % (c["status"].upper(), c["id"], c["measured"], c["baseline"]))
        print("       fix: %s" % c["remediation"])
    elif c["status"] == "skipped":
        print("  [SKIP] %s: %s" % (c["id"], c["reason"]))
if fails:
    raise SystemExit(1)
PY
PYRC=$?
if [[ "$PYRC" -ne 0 ]]; then FAILS=$((FAILS+1)); fi

printf '\nsmoke: %d failure group(s)\n' "$FAILS"
if [[ "$FAILS" -gt 0 ]]; then
  printf 'SMOKE TEST RESULT: FAIL\n'
  exit 1
fi
printf 'SMOKE TEST RESULT: PASS\n'
exit 0
