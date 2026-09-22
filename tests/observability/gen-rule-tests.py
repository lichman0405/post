#!/usr/bin/env python3
"""Turn metrics really scraped off running POST processes into a promtool
unit-test file.

WHY THIS EXISTS

"the alert fires" is worth nothing if the series it fires on were invented.
This script reads the raw Prometheus text bodies that
tests/observability/metrics-alerts-e2e.sh captured from live cmd/api and
cmd/worker processes and emits them as promtool `input_series`, so the
assertions promtool checks are made against values that a real process really
reported, at the real cadence.

What is real and what is not, stated once so no reader has to guess:

  * The VALUES are real. Every number in the emitted input_series was read out
    of a scrape body captured during the run. Nothing is interpolated or
    smoothed: one scrape becomes one sample.
  * The SPACING is real. The harness scrapes on a fixed 15s grid and the test
    declares interval: 15s, so both the sample count and the elapsed test
    clock match the run.
  * A MISSING scrape is emitted as `_`, not as a carried-forward value. If the
    process was dead at that instant, the test is told the series was absent —
    which is what Prometheus would have seen, and is the only honest input for
    the absent() rule. A slot the harness never scraped at all (no file for
    either process) adds no row at all: the series ends where the run ended,
    and the count printed below is the number of scrapes really taken.
  * Only series this file lists are emitted. A rule whose series the harness
    could not capture is NOT given a fabricated series here; those rules are
    asserted elsewhere and the harness prints which ones were not real.

The expected ALERTS label sets are derived from the rule file and the input
series (alertname + the rule's static labels + the labels the expression
keeps), not copied from promtool's output. Copying what the tool says would
make the test agree with the tool by construction and prove nothing.

Usage:
  gen-rule-tests.py --samples DIR --rules FILE --output FILE \
      [--expect name=EVAL_TIME:state ...]
"""

from __future__ import annotations

import argparse
import os
import re
import sys

# ---------------------------------------------------------------------------
# Which process reports which series.
#
# This is a fact about the platform, not a convenience: post_db_up is emitted
# by both binaries on their own endpoints, so the generator has to be told
# which process's copy to use rather than taking whichever file it opens
# first. The worker is used for everything the worker's own loops and
# collectors produce; the API for the HTTP surface and the reconciliation
# sweep it runs.
# ---------------------------------------------------------------------------
SERIES_SOURCE = {
    "post_db_up": "worker",
    "post_queue_errors_total": "worker",
    "post_queue_depth": "worker",
    "post_outbox_publish_failures_total": "worker",
    "post_outbox_oldest_pending_seconds": "worker",
    "post_outbox_pending_events": "worker",
    "post_http_requests_total": "api",
    "post_permission_denials_total": "api",
    "post_rsg_reconciliation_open_findings": "api",
    "post_rsg_reconciliation_passes_total": "api",
    "post_metrics_collector_errors_total": "worker",
}

# Static labels each rule attaches, read from ops/observability/alerts.yml.
# Kept here rather than parsed out of the YAML so that a change to the rule
# file that this table does not know about shows up as a test failure instead
# of silently relaxing the expectation.
RULE_SEVERITY = {
    "PostDatabaseUnavailable": "P1",
    "PostRSGGitDrift": "P1",
    "PostMetricsEndpointMissing": "P1",
    "PostOutboxBacklog": "P2",
    "PostOutboxPublishFailing": "P2",
    "PostSearchUnavailable": "P2",
    "PostSearchLatencySlow": "P2",
    "PostWebhookFailuresSurge": "P2",
    "PostJobQueueUnavailable": "P2",
    "PostJobDeadLettered": "P2",
    "PostPermissionDenialsElevated": "P2",
}

# The expected ALERTS label set of a firing rule is derived, not copied from
# promtool: alertname + alertstate, plus the labels the input series carries
# that the expression's selector pins (an `increase(post_queue_errors_total{
# operation="read"}[5m])` alert keeps operation="read"; a bare `post_db_up`
# keeps nothing), plus the rule's own static labels. Deriving it means a
# change to a rule that this file does not know about fails the test instead
# of quietly agreeing with whatever the rule now does.


class Sample:
    """One scrape body."""

    def __init__(self) -> None:
        self.series: dict[tuple[str, tuple[tuple[str, str], ...]], float] = {}

    def get(self, name: str, labels: dict[str, str] | None = None):
        key = (name, tuple(sorted((labels or {}).items())))
        return self.series.get(key)


SERIES_RE = re.compile(
    r"""^(?P<name>[a-zA-Z_:][a-zA-Z0-9_:]*)     # metric name
         (?:\{(?P<labels>.*)\})?                # optional label block
         [\ \t]+(?P<value>[^\ \t]+)             # value
         (?:[\ \t]+(?P<ts>[^\ \t]+))?$          # optional timestamp
     """,
    re.VERBOSE,
)
LABEL_RE = re.compile(r'([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"')


def parse_prom(text: str) -> Sample:
    s = Sample()
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        m = SERIES_RE.match(line)
        if not m:
            continue
        labels: dict[str, str] = {}
        if m.group("labels"):
            for lm in LABEL_RE.finditer(m.group("labels")):
                labels[lm.group(1)] = lm.group(2).replace('\\"', '"').replace("\\\\", "\\")
        try:
            value = float(m.group("value"))
        except ValueError:
            continue  # NaN / +Inf are not values this test needs
        s.series[(m.group("name"), tuple(sorted(labels.items())))] = value
    return s


def load_samples(samples_dir: str, limit: int | None = None):
    """Returns the api and worker scrape sequences, ordered by index.

    A missing or unreadable file yields None for that step, which becomes `_`
    in the emitted series.

    A step with NO file at all for EITHER process is not a step: the loop
    stops there and no row is emitted for it. That is the difference between
    "the scrape happened and this process was not answering" (a `_`, which is
    what a real Prometheus would have recorded) and "nobody scraped" (nothing,
    because no sample was taken). The earlier version appended the empty row
    before noticing, which put one extra `_` on the end of every series and
    made the count this script prints one larger than the number of scrapes
    the harness actually took — a number that then disagreed with the
    harness's own timeline.
    """
    api: list[Sample | None] = []
    worker: list[Sample | None] = []
    for i in range(limit if limit is not None else 10_000):
        steps = [(api, os.path.join(samples_dir, f"api-{i:03d}.prom")),
                 (worker, os.path.join(samples_dir, f"worker-{i:03d}.prom"))]
        if not any(os.path.exists(path) for _, path in steps):
            break
        for seq, path in steps:
            if not os.path.exists(path):
                seq.append(None)
                continue
            try:
                with open(path, encoding="utf-8") as fh:
                    body = fh.read()
            except OSError:
                seq.append(None)
                continue
            # A zero-byte body is a scrape that reached the port but got
            # nothing useful; treat it as absent rather than as an empty
            # metrics page, which would silently zero every series.
            seq.append(parse_prom(body) if body.strip() else None)
    return api, worker


def collect(seq, name: str, labels: dict[str, str] | None) -> list[str]:
    out: list[str] = []
    for s in seq:
        if s is None:
            out.append("_")
            continue
        v = s.get(name, labels)
        out.append("_" if v is None else format_value(v))
    return out


def format_value(v: float) -> str:
    if v == int(v) and abs(v) < 1e15:
        return str(int(v))
    return repr(v)


# Which labels of the input series SURVIVE into the alert. This is decided by
# the rule's expression and nothing else: a bare selector keeps its labels
# (`increase(post_queue_errors_total{operation="read"}[5m])` pins operation),
# while any aggregation drops them (`sum(rate(post_permission_denials_total
# [10m]))` keeps none — the sum has no such label to carry). Getting this
# wrong in the direction of "expected too many labels" fails loudly, which is
# the safe direction.
RULE_KEPT_LABELS = {
    "PostDatabaseUnavailable": [],
    "PostJobQueueUnavailable": ["operation"],
    "PostOutboxPublishFailing": [],
    "PostOutboxBacklog": [],
    "PostMetricsEndpointMissing": [],
    "PostRSGGitDrift": [],
    "PostPermissionDenialsElevated": [],   # sum(rate(...)) keeps no labels
}


def alerts_labels(alertname: str, inputs: list[tuple[str, dict[str, str]]], state: str) -> str:
    keep = RULE_KEPT_LABELS.get(alertname)
    if keep is None:
        raise SystemExit(f"gen-rule-tests: no RULE_KEPT_LABELS entry for {alertname}; "
                         f"add one rather than guessing which labels survive")
    labels = {"alertname": alertname, "alertstate": state}
    for _, lab in inputs:
        for k, v in lab.items():
            if k in keep:
                labels[k] = v
    labels["severity"] = RULE_SEVERITY[alertname]
    parts = ",".join(f'{k}="{v}"' for k, v in sorted(labels.items()))
    return f"ALERTS{{{parts}}}"


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--samples", required=True)
    ap.add_argument("--rules", required=True)
    ap.add_argument("--output", required=True)
    ap.add_argument("--expect", action="append", default=[],
                    help="alertname=EVAL_TIME:1 (firing), :0 (no ALERTS series at all) "
                         "or :2 (pending — the condition holds but the rule's `for:` has not elapsed)")
    ap.add_argument("--note", default="")
    args = ap.parse_args()

    api, worker = load_samples(args.samples)
    steps = max(len(api), len(worker))
    if steps == 0:
        print("gen-rule-tests: no samples found", file=sys.stderr)
        return 1

    src = {"api": api, "worker": worker}

    # Every series any assertion needs, gathered once.
    needed: list[tuple[str, dict[str, str]]] = []
    for name, labels in [
        ("post_db_up", {}),
        ("post_queue_errors_total", {"operation": "read"}),
        ("post_outbox_publish_failures_total", {}),
        ("post_outbox_oldest_pending_seconds", {}),
        ("post_outbox_pending_events", {}),
        ("post_http_requests_total", {"method": "POST", "route": "POST /internal/jobs", "status": "503"}),
        ("post_permission_denials_total", {"decision": "forbidden", "surface": "api"}),
        ("post_rsg_reconciliation_open_findings", {}),
    ]:
        if name in SERIES_SOURCE:
            needed.append((name, labels))

    input_series = []
    for name, labels in needed:
        vals = collect(src[SERIES_SOURCE[name]], name, labels)
        if all(v == "_" for v in vals):
            # Never reported by this process at any point in the run. Emitting
            # an all-absent series would be a fabrication that the rules file
            # then evaluates against; say so instead.
            print(f"gen-rule-tests: NOTE {name}{labels} was never captured; omitted", file=sys.stderr)
            continue
        lab = "".join(f'{k}="{v}",' for k, v in sorted(labels.items()))
        input_series.append((f"{name}{{{lab}}}", vals))

    lines: list[str] = []
    lines.append("# GENERATED by tests/observability/gen-rule-tests.py — do not edit by hand.")
    lines.append("#")
    lines.append("# input_series below are the raw values scraped from live cmd/api and")
    lines.append("# cmd/worker processes during this run, one sample per scrape, `_` where the")
    lines.append("# process was not answering. See the generator's docstring for what that does")
    lines.append("# and does not prove.")
    if args.note:
        for l in args.note.splitlines():
            lines.append(f"# {l}")
    lines.append("")
    lines.append("rule_files:")
    lines.append(f"  - {os.path.abspath(args.rules)}")
    lines.append("")
    lines.append("evaluation_interval: 15s")
    lines.append("")
    lines.append("tests:")
    lines.append("  - interval: 15s")
    lines.append("    input_series:")
    for name, vals in input_series:
        lines.append(f"      - series: '{name}'")
        lines.append(f"        values: '{' '.join(vals)}'")
    lines.append("    promql_expr_test:")

    # The negative control is listed first on purpose: if a rule is going to be
    # wrong in the direction of always firing, the reader sees that before
    # seeing the green positive.
    expects = []
    for e in args.expect:
        alertname, rest = e.split("=", 1)
        eval_time, state = rest.rsplit(":", 1)
        expects.append((alertname, eval_time, int(state)))
    expects.sort(key=lambda t: {0: 0, 2: 1, 1: 2}[t[2]])  # quiet, then pending, then firing

    # Three states, and they are three different claims about one rule:
    #
    #   0  the alert is in NO state at all — the series does not exist. This is
    #      the negative control: a rule that always fires fails here.
    #   1  the alert is FIRING.
    #   2  the alert is PENDING: the expression has been true for less than the
    #      rule's `for:`, so Prometheus is holding it. Asserting this is how a
    #      test says "the threshold was crossed, and the hold is what kept the
    #      alert quiet" — which is strictly more than "it had not fired yet".
    #      The pending series disappears once the alert fires, so :2 and :1 at
    #      two eval times are also evidence that the transition happened.
    #
    # The state is part of the SELECTOR for 2 (so the assertion is about the
    # pending series specifically) and the `0` expectation deliberately keeps
    # the alertname-only selector: "no series at all" is the strongest form of
    # "did not fire", and a caller that expects a pending state must say so
    # with :2 rather than have :0 quietly tolerate it.
    for alertname, eval_time, state in expects:
        if state == 1:
            what, selector = "FIRING", f'ALERTS{{alertname="{alertname}"}}'
        elif state == 2:
            what = "PENDING (condition true, `for:` not yet elapsed)"
            selector = f'ALERTS{{alertname="{alertname}",alertstate="pending"}}'
        else:
            what, selector = "NOT firing", f'ALERTS{{alertname="{alertname}"}}'
        lines.append(f"      # {alertname}: expect {what} at {eval_time}")
        lines.append(f"      - expr: '{selector}'")
        lines.append(f"        eval_time: {eval_time}")
        if state:
            inputs = [(n, l) for n, l in needed if n == ALERT_SERIES.get(alertname, "")]
            lines.append("        exp_samples:")
            lines.append(f"          - labels: '{alerts_labels(alertname, inputs, 'firing' if state == 1 else 'pending')}'")
            lines.append("            value: 1")
        else:
            lines.append("        exp_samples: []")

    with open(args.output, "w", encoding="utf-8") as fh:
        fh.write("\n".join(lines) + "\n")
    # The census is counted here, from the same list the assertions were
    # written from, and printed. A number in a README is a claim someone has
    # to re-count; a number in this line is reproduced by every run, and it
    # cannot drift from the file because both come from `expects`. The three
    # states are counted separately: "not firing" (the negative control, no
    # ALERTS series at all) and "pending" are different claims about a rule
    # (see the block above) and a total that lumps them cannot be checked
    # against either.
    counts = {0: 0, 1: 0, 2: 0}
    for _, _, state in expects:
        counts[state] += 1
    print(f"gen-rule-tests: wrote {args.output} ({steps} scrapes, "
          f"{len(input_series)} series, {len(expects)} assertions: "
          f"{counts[1]} firing, {counts[0]} not-firing (exp_samples: []), "
          f"{counts[2]} pending)")
    return 0


# Which input series each alert's labels come from, so the expected ALERTS
# label set can be derived rather than guessed.
ALERT_SERIES = {
    "PostDatabaseUnavailable": "post_db_up",
    "PostJobQueueUnavailable": "post_queue_errors_total",
    "PostOutboxPublishFailing": "post_outbox_publish_failures_total",
    "PostOutboxBacklog": "post_outbox_oldest_pending_seconds",
    "PostMetricsEndpointMissing": "post_db_up",
    "PostRSGGitDrift": "post_rsg_reconciliation_open_findings",
    "PostPermissionDenialsElevated": "post_permission_denials_total",
}


if __name__ == "__main__":
    sys.exit(main())
