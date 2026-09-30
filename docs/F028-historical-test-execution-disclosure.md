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

The preceding worker reported later local verification activity, including:

- a fake-only run of `tests/test_dev_tailscale.py` with the hosted guard enabled
  but `TMPDIR=/private/var/tmp` (not the historical `TMPDIR=/tmp` command above);
- `go test -count=1 ./internal/tailscalecli ./internal/config ./cmd/herdr-mobile-relay`;
- `go test -race -count=1 ./internal/tailscalecli ./internal/config ./cmd/herdr-mobile-relay`;
- `go test -count=1 ./internal/app`, targeted app cases, `go vet` for the
  affected packages, and focused supported tagged app fixtures;
- shell syntax/static checks and `make production-path-audit`; and
- an attempted local `make shell-check`, which did not complete on Darwin. The
  default temporary path first exceeded AF_UNIX path limits; with a shorter
  temporary path, the then-current `tests/test_tailscale_cli_setup_failure.py`
  fixture invoked GNU `stat -c`, which is incompatible with BSD `stat`. That
  fixture has since been updated to emulate both GNU and BSD `stat` forms; the
  failed historical check remains failed and has not been rerun locally.

These are reported follow-on activities, not primary transcripts preserved with
this disclosure, and they do not establish the outcomes of either historical
command. In particular, the similar guarded test run used a different `TMPDIR`
and cannot substitute for the original invocation. None of the reported results
qualifies the current architecture or current candidate SHA.

The owner authorized continued technical review after this durable disclosure.
That authorization is not retroactive test authorization and does not erase or
close F028. Future verification must use the repository's configured hosted CI;
workers must not recreate hosted-only environment overrides locally. No
process-compliance claim should be inferred from this disclosure.

F028 remains open. Record the historical outcomes only if the contemporaneous
primary logs or transcripts are located; otherwise retain them as unknown.
