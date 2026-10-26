# Worker recovery drill

This directory is the controlled stop/respawn sample for WEB-28 (T1334).
The Worker writes a minimal Go test here; the Supervisor stops the first
attempt mid-run and respawns it from disk state. It is not product code.

`recovery_test.go` re-executes the test binary as real child processes rather
than checking JSON the test wrote itself:

- Attempt 1 (`hold`) records itself as running and writes a fresh random
  nonce into the state file, optionally starts a detached (`setsid`)
  grandchild, and blocks. The parent confirms both pids are alive with raw
  `syscall.Kill(pid, 0)` and `/proc` checks.
- The stop kills attempt 1's whole process group *and* the grandchild,
  which deliberately left that group. The parent polls raw pid liveness
  until both are gone and only then reaps attempt 1; the hold (60s) outlives
  the stop deadline (5s), so a stop that misses attempt 1 is observed as a
  live process and fails the test.
- Attempt 2 (`resume`) must read attempt 1's state, echo its nonce into the
  new state and write the probe from it. The parent builds the expected probe
  from the nonce it read off disk after attempt 1, so a from-scratch restart
  that invents `attempt=2` cannot pass.
- `TestRespawnedAttemptWritesProbeFromDiskState` covers the probe half;
  `TestStopLeavesNoLiveProcessAndRespawnStartsANewAttempt` covers the
  stop/new-attempt half.

The probe is generated at test time, so no fixture committed with the patch
can satisfy it. The boundary: this proves the mechanics a respawned process
needs on Linux (process groups, `/proc`, pid liveness). The real
`rddev worker stop` / `rddev worker respawn` around this Worker and the final
collect are Supervisor-side evidence, not something this package asserts.

Run it with:

    go test ./tests/worker-recovery/
