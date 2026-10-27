/**
 * Step accounting for the WEB-01 journey (docs/70 §6.4).
 *
 * The acceptance rule is not "the run reached the end"; it is "every step
 * was executed". A helper that quietly skips a step while printing a green
 * line is exactly the failure this module exists to make impossible:
 *
 *   - a step that throws is recorded `failed` and the journey aborts;
 *   - a step name outside the declared list is rejected (a step nobody
 *     planned is not evidence for a planned one);
 *   - a duplicate step is rejected (running one step twice does not stand
 *     in for running another);
 *   - assertComplete() fails when ANY declared step has no `passed` record,
 *     including the zero-steps case.
 *
 * The declared list is the contract: the journey names its steps up front,
 * and the evidence records which ones actually ran.
 */

export class StepError extends Error {
  constructor(message) {
    super(message);
    this.name = "StepError";
  }
}

export class JourneySteps {
  /**
   * @param {string} journey name used in messages and evidence
   * @param {string[]} expected every step the journey promises to run
   */
  constructor(journey, expected) {
    if (!Array.isArray(expected) || expected.length === 0) {
      throw new StepError(`journey ${journey}: declared no steps — a journey with no steps proves nothing`);
    }
    if (new Set(expected).size !== expected.length) {
      throw new StepError(`journey ${journey}: the declared step list contains duplicates`);
    }
    this.journey = journey;
    this.expected = [...expected];
    /** @type {Map<string, {name: string, status: string, startedAt: string, finishedAt: string, error?: string}>} */
    this.records = new Map();
    this.startedAt = new Date().toISOString();
  }

  /**
   * Run one declared step and record the observed outcome.
   * @template T
   * @param {string} name
   * @param {() => Promise<T>} fn
   * @returns {Promise<T>}
   */
  async step(name, fn) {
    if (!this.expected.includes(name)) {
      throw new StepError(`journey ${this.journey}: step "${name}" was not declared; add it to the expected list or stop running it`);
    }
    if (this.records.has(name)) {
      throw new StepError(`journey ${this.journey}: step "${name}" ran twice`);
    }
    const startedAt = new Date().toISOString();
    try {
      const value = await fn();
      this.records.set(name, { name, status: "passed", startedAt, finishedAt: new Date().toISOString() });
      return value;
    } catch (err) {
      this.records.set(name, {
        name,
        status: "failed",
        startedAt,
        finishedAt: new Date().toISOString(),
        error: errorText(err),
      });
      throw err;
    }
  }

  /** True only when every declared step ran and passed. */
  isComplete() {
    return this.expected.every((name) => this.records.get(name)?.status === "passed");
  }

  /**
   * Fail when any declared step did not run, or did not pass. This is the
   * last assertion of a journey: reaching the end of the script is not the
   * same as having executed the contract.
   */
  assertComplete() {
    const missing = this.expected.filter((name) => !this.records.has(name));
    const failed = this.expected.filter((name) => this.records.get(name)?.status === "failed");
    const notPassed = this.expected.filter((name) => {
      const record = this.records.get(name);
      return record !== undefined && record.status !== "passed";
    });
    if (missing.length > 0 || failed.length > 0 || notPassed.length > 0) {
      const parts = [];
      if (missing.length > 0) parts.push(`did not run: ${missing.join(", ")}`);
      if (failed.length > 0) parts.push(`failed: ${failed.join(", ")}`);
      const other = notPassed.filter((name) => !failed.includes(name));
      if (other.length > 0) parts.push(`not passed: ${other.join(", ")}`);
      throw new StepError(`journey ${this.journey}: ${parts.join("; ")} — an incomplete run is not a pass`);
    }
  }

  /** The evidence record, in declaration order. */
  toJSON() {
    return {
      journey: this.journey,
      startedAt: this.startedAt,
      finishedAt: new Date().toISOString(),
      expected: this.expected,
      steps: this.expected.map((name) => this.records.get(name) ?? {
        name,
        status: "not_run",
        startedAt: null,
        finishedAt: null,
      }),
      complete: this.isComplete(),
    };
  }
}

export function errorText(err) {
  if (err instanceof Error) return err.message;
  return String(err);
}
