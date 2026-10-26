# Worker precheck

This directory is the controlled sample for WEB-28 (T1333). It exists so the
Worker's allowed_scope has a real path before the probe test is written; the
Worker adds the Go test package here and runs it under G1.

It is not product code and makes no claim about the platform's behaviour.

`precheck_test.go` asserts the byte-for-byte content of `testdata/probe.txt`,
which the Worker created inside its own worktree. Before that file exists the
test fails with "no such file or directory"; after it exists the test passes:

    go test ./tests/worker-precheck/

The expected bytes live in the test rather than being read back from the probe,
so the assertion can actually fail. This sample is one slice of WEB-28: the
Reviewer, failed-rework and disk-recovery paths are checked separately.
