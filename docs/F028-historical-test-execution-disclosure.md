# F028: historical test-execution disclosure

F028 records historical execution of these commands in this checkout:

```text
TMPDIR=/tmp HERDR_TAILSCALE_LAUNCHER_CI=1 python3 tests/test_dev_tailscale.py
go test ./internal/deviceauth
```

The first command sets `HERDR_TAILSCALE_LAUNCHER_CI=1`, a guard intended for a
hosted-only lifecycle test. This disclosure does not claim that the execution
was authorized hosted CI, nor that either result is evidence for the current
revision. The historical result of each listed command remains unknown because
no contemporaneous exit code, stdout/stderr, or test summary was located. Do not
infer a pass or failure from later or similarly named runs.

## Subsequent local activity reported

The preceding worker reported these later local commands or command groups. The
worker's report is secondary evidence; no complete primary transcript, exact
exit code, or command-level test summary was preserved for the listed Go test
runs or fixture run. Therefore their outcomes are recorded as **not verifiable
from retained evidence**, not as passes. Exact `TMPDIR` values not stated below
were not retained and must not be reconstructed by guesswork.

| Reported invocation | Reported outcome and evidence limits |
| --- | --- |
| `TMPDIR=/private/var/tmp HERDR_TAILSCALE_LAUNCHER_CI=1 python3 tests/test_dev_tailscale.py` | Reported as a fake-only local fixture run. Its exit code and stdout/stderr were not preserved, so a pass/failure cannot be verified. It used `/private/var/tmp`, not `/tmp`, and deliberately set the hosted-only guard locally; it is prohibited local execution, not hosted evidence, and does not establish the original command's result. The fake-only description is a worker report, not a retained process trace. Do not rerun it. |
| `go test -count=1 ./internal/tailscalecli ./internal/config ./cmd/herdr-mobile-relay` | Reported as executed locally; command-level exit code and output were not retained. `TMPDIR` was not recorded. These tests execute project code and are not evidence for the exact final SHA. |
| `go test -race -count=1 ./internal/tailscalecli ./internal/config ./cmd/herdr-mobile-relay` | Reported as executed locally; command-level exit code and output were not retained. `TMPDIR` was not recorded. These tests execute project code and are not evidence for the exact final SHA. |
| `go test -count=1 ./internal/app` and targeted app cases | Reported as executed locally, but the targeted selectors, exit codes, output, and `TMPDIR` were not retained. Treat every outcome as unverified. |
| Focused tagged app fixtures | Reported as executed locally, but the exact build/test command, tag set, exit code, output, and `TMPDIR` were not retained. Treat every outcome as unverified. |
| `make shell-check` with inherited/default `TMPDIR` | Reported failure: the test's Unix-socket path exceeded the platform path limit. The effective `TMPDIR`, exact command transcript, and exit code were not retained. |
| A later `make shell-check` attempt using a shorter `TMPDIR` | Reported failure: the then-current fixture called GNU `stat -c`, which is incompatible with BSD `stat`. The exact `TMPDIR`, command transcript, and exit code were not retained. The fixture was later changed to emulate GNU and BSD forms; this historical failed check was not rerun. |

Other reported follow-on checks included affected-package `go vet`, shell
syntax/static checks, and `make production-path-audit`. Their exact commands,
`TMPDIR` values, and primary outputs were not preserved in this disclosure.
They do not establish the results of the historical commands above. No result
from the later `/private/var/tmp` command substitutes for the original `/tmp`
invocation.

## Current handling and evidence boundary

The owner authorized continued technical review after this durable disclosure.
That authorization is not retroactive test authorization and does not erase or
close F028. No worker may recreate a hosted-only environment override locally or
repeat the historical lifecycle commands. Local verification for the current
remediation is restricted to source inspection, static analysis, build, and
compile-only checks; actual tests and lifecycle workflows belong to hosted CI
for the exact final SHA.

A successful hosted run on a later SHA is revision-specific evidence and cannot
recover missing historical local stdout, stderr, exit codes, test summaries, or
side-effect traces. No process-compliance claim should be inferred from this
disclosure. F028 remains open. Record historical outcomes only if contemporaneous
primary logs or transcripts are located; otherwise retain them as unknown.
