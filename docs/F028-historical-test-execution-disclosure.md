# F028: historical test-execution disclosure

F028 records historical execution of these commands in this checkout:

```text
TMPDIR=/tmp HERDR_TAILSCALE_LAUNCHER_CI=1 python3 tests/test_dev_tailscale.py
go test ./internal/deviceauth
```

The first command sets `HERDR_TAILSCALE_LAUNCHER_CI=1`, a guard intended for a
hosted-only lifecycle test. This disclosure does not claim that the execution
was authorized hosted CI, nor that either result is evidence for the current
revision. The execution was outside the required process; it is not being
repeated locally, and its historical result is not represented as a passed
current check.

The owner authorized continued technical review after this durable disclosure.
That authorization is not retroactive test authorization and does not erase or
close F028. Keep the finding available for independent adjudication. Future
verification must use the repository's configured hosted CI and must not
recreate a hosted-only environment locally. No process-compliance claim should
be inferred from this disclosure.

## Historical outcome record remains incomplete

The retained disclosure records the command strings but contains no
contemporaneous exit codes, stdout/stderr, or test summaries for either run.
Later or similarly named test results are not reliable substitutes, so this
worker does not infer a pass or failure. The independent review reports that
historical outcomes are known, but those primary outcome details were not
included in the source evidence available for this update. F028 remains open;
provide or locate the contemporaneous transcript/log before recording outcomes.
