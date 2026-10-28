/**
 * Evidence recording for the WEB-01 journey (docs/70 §6.4).
 *
 * Every run writes, under its own run-id directory:
 *
 *   metadata.json   commit, runtime versions, environment, run id, role,
 *                   entry URL, account input summary, expected/actual
 *                   result and timings;
 *   steps.json      which declared steps ran, in order, and their outcome;
 *   network.jsonl   every browser request/response the journey observed
 *                   (method, URL, status, resource type) — never a request
 *                   body, never a cookie value;
 *   console.jsonl   browser console messages and page errors;
 *   *.png           key-step screenshots;
 *   trace.zip       the Playwright trace (written by the journey).
 *
 * Redaction is structural: this module never receives the account password
 * and records no cookie values. A trace records the session cookie because
 * a browser trace must, which is why it is only produced against this
 * synthetic, isolated stack.
 *
 * Evidence write failures are failures: a screenshot, trace or metadata
 * file that cannot be written must fail the run, never degrade to a
 * warning. A green run whose trace is missing is indistinguishable from a
 * fabricated one.
 */

import { mkdir, stat, writeFile } from "node:fs/promises";
import path from "node:path";

function iso() {
  return new Date().toISOString();
}

export class Evidence {
  constructor(options) {
    this.runId = options.runId;
    this.outDir = options.outDir;
    this.commit = options.commit;
    this.apiOrigin = options.apiOrigin ?? "";
    this.webOrigin = options.webOrigin ?? "";
    this.entryUrl = options.entryUrl ?? "";
    this.journeyName = options.journeyName ?? "journey";
    // Which task slice the journey belongs to. The metadata names it so a
    // reviewer of a run's evidence can tell the WEB-02 and WEB-03 journeys
    // apart without guessing from the file path.
    this.task = options.task ?? "T1302";
    this.accountEmail = options.accountEmail ?? "";
    this.role = options.role ?? "";
    this.expectedSteps = options.expectedSteps ?? [];
    this.startedAt = iso();
    this.console = [];
    this.network = [];
    this.screenshots = [];
    this.pageErrors = [];
    this.failedRequests = [];
    this.serverErrors = [];
  }

  async init() {
    await mkdir(this.outDir, { recursive: true });
  }

  addConsole(kind, text, location) {
    this.console.push({ at: iso(), kind, text, location: location ?? null });
  }

  addPageError(message, stack) {
    const entry = { at: iso(), kind: "pageerror", text: message, stack: stack ?? null };
    this.pageErrors.push(entry);
    this.console.push(entry);
  }

  addRequest(request) {
    this.network.push({
      at: iso(),
      event: "request",
      method: request.method(),
      url: request.url(),
      resourceType: request.resourceType(),
      navigation: request.isNavigationRequest(),
    });
  }

  addResponse(response) {
    const request = response.request();
    this.network.push({
      at: iso(),
      event: "response",
      method: request.method(),
      url: response.url(),
      status: response.status(),
      resourceType: request.resourceType(),
    });
    if (response.status() >= 500) {
      this.serverErrors.push({ at: iso(), method: request.method(), url: response.url(), status: response.status() });
    }
  }

  addFailedRequest(request, failure) {
    const errorText = failure?.errorText ?? "unknown failure";
    this.network.push({
      at: iso(),
      event: "requestfailed",
      method: request.method(),
      url: request.url(),
      resourceType: request.resourceType(),
      errorText,
    });
    // Navigation cancellations and aborted prefetches are browser
    // mechanics, not a broken product step; every other failure is a
    // failed request and the journey reports it.
    const aborted = errorText.includes("ERR_ABORTED");
    const favicon = request.url().endsWith("/favicon.ico");
    if (!aborted && !favicon) {
      this.failedRequests.push({ at: iso(), method: request.method(), url: request.url(), errorText });
    }
  }

  /** One PNG per key step, named in execution order. */
  async screenshot(page, label) {
    const safe = String(label).replace(/[^a-zA-Z0-9_-]+/g, "-").replace(/^-+|-+$/g, "");
    const file = path.join(this.outDir, `${String(this.screenshots.length + 1).padStart(2, "0")}-${safe}.png`);
    await page.screenshot({ path: file, fullPage: true });
    this.screenshots.push({ label, file });
    return file;
  }

  /**
   * Require an evidence artifact to exist and be non-empty. A trace.zip
   * that Playwright did not write (or wrote empty) must fail the run: a
   * green result pointing at missing evidence cannot be reviewed.
   */
  async verifyArtifact(file, what) {
    let info;
    try {
      info = await stat(file);
    } catch (err) {
      throw new Error(`${what} was not written at ${file}: ${err instanceof Error ? err.message : String(err)}`);
    }
    if (!info.isFile() || info.size === 0) {
      throw new Error(`${what} at ${file} is missing or empty`);
    }
    return info.size;
  }

  /**
   * Write the evidence files. `steps` is a JourneySteps instance (or its
   * toJSON() output). `failure` is the aborting error, if any.
   */
  async finalize({ steps, result, failure, browserVersion, playwrightVersion, harness }) {
    const stepsRecord = typeof steps.toJSON === "function" ? steps.toJSON() : steps;
    const finishedAt = iso();
    const metadata = {
      harness: "tests/web-v2",
      gate: "G23",
      task: this.task,
      runId: this.runId,
      commit: this.commit,
      startedAt: this.startedAt,
      finishedAt,
      environment: {
        node: process.version,
        playwright: playwrightVersion ?? "unknown",
        browser: browserVersion ?? "unknown",
        platform: process.platform,
        arch: process.arch,
      },
      apiOrigin: this.apiOrigin,
      webOrigin: this.webOrigin,
      harness,
      role: this.role,
      entryUrl: this.entryUrl,
      account: {
        // Synthetic address only. The password is deliberately absent from
        // every evidence file and log.
        email: this.accountEmail,
        password: "<redacted: synthetic test credential never recorded>",
      },
      journey: this.journeyName,
      expectedSteps: this.expectedSteps,
      result,
      failure: failure === undefined ? null : String(failure?.stack ?? failure),
      evidence: {
        screenshots: this.screenshots,
        trace: path.join(this.outDir, "trace.zip"),
        console: path.join(this.outDir, "console.jsonl"),
        network: path.join(this.outDir, "network.jsonl"),
        steps: path.join(this.outDir, "steps.json"),
      },
      observations: {
        pageErrors: this.pageErrors,
        failedRequests: this.failedRequests,
        serverErrors: this.serverErrors,
        consoleErrors: this.console.filter((entry) => entry.kind === "error"),
      },
    };

    await writeFile(path.join(this.outDir, "steps.json"), JSON.stringify(stepsRecord, null, 2) + "\n");
    await writeFile(
      path.join(this.outDir, "network.jsonl"),
      this.network.map((entry) => JSON.stringify(entry)).join("\n") + (this.network.length > 0 ? "\n" : ""),
    );
    await writeFile(
      path.join(this.outDir, "console.jsonl"),
      this.console.map((entry) => JSON.stringify(entry)).join("\n") + (this.console.length > 0 ? "\n" : ""),
    );
    await writeFile(path.join(this.outDir, "metadata.json"), JSON.stringify(metadata, null, 2) + "\n");
    return metadata;
  }
}
