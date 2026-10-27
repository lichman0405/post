import test from "node:test";
import assert from "node:assert/strict";

import { JourneySteps, StepError, errorText } from "./steps.mjs";

test("a complete journey passes", async () => {
  const steps = new JourneySteps("unit", ["one", "two"]);
  await steps.step("one", async () => {});
  await steps.step("two", async () => {});
  steps.assertComplete();
  assert.equal(steps.isComplete(), true);
  const record = steps.toJSON();
  assert.deepEqual(record.steps.map((s) => s.status), ["passed", "passed"]);
});

test("a skipped declared step fails assertComplete, not silently passes", async () => {
  const steps = new JourneySteps("unit", ["one", "two"]);
  await steps.step("one", async () => {});
  assert.equal(steps.isComplete(), false);
  assert.throws(() => steps.assertComplete(), /did not run: two/);
  const record = steps.toJSON();
  assert.equal(record.steps[1].status, "not_run");
});

test("zero executed steps fails", async () => {
  const steps = new JourneySteps("unit", ["one"]);
  assert.throws(() => steps.assertComplete(), /did not run: one/);
});

test("a journey with no declared steps cannot be constructed", () => {
  assert.throws(() => new JourneySteps("unit", []), StepError);
});

test("an undeclared step is rejected", async () => {
  const steps = new JourneySteps("unit", ["one"]);
  await assert.rejects(async () => {
    await steps.step("surprise", async () => {});
  }, /was not declared/);
});

test("a duplicate step is rejected", async () => {
  const steps = new JourneySteps("unit", ["one", "two"]);
  await steps.step("one", async () => {});
  await assert.rejects(async () => {
    await steps.step("one", async () => {});
  }, /ran twice/);
});

test("a throwing step is recorded failed and aborts", async () => {
  const steps = new JourneySteps("unit", ["one", "two"]);
  await assert.rejects(async () => {
    await steps.step("one", async () => {
      throw new Error("primary button did not reach a signed-in home page");
    });
  }, /primary button did not reach/);
  const record = steps.toJSON();
  assert.equal(record.steps[0].status, "failed");
  assert.match(record.steps[0].error, /primary button did not reach/);
  assert.throws(() => steps.assertComplete(), /failed: one/);
});

test("errorText describes non-Error values", () => {
  assert.equal(errorText("plain"), "plain");
  assert.equal(errorText(new Error("boom")), "boom");
});
