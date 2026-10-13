# Native macOS development loop

## 1. Use the Mac where it adds value

The current failure lives in stock shell utilities/launcher behavior, not Go cross-compilation. Use a native Mac to observe that behavior **before** another push. Prefer Apple Silicon initially because the existing Darwin lifecycle driver explicitly supports only Darwin/arm64; Intel macOS must get its own native evidence before claiming Intel support.

Maintain three loops:

| Loop | Where | When | Purpose |
|---|---|---|---|
| Edit → focused unit/parser/fixture | Mac local workspace; safe fixture runner | Every substep | Tight feedback, one failure at a time. |
| Native lifecycle/package + portable regressions | Disposable native macOS guest/runner; Linux VM on same Mac or dedicated Linux CI | Before feature checkpoint | Real kernel/Bash/BSD behavior and Linux regressions. |
| Exact-commit cross-platform CI + browser/tailnet acceptance | Authorized CI/disposable environments | At milestones/final freeze | Reproduce independently, not replace local debugging. |

A Linux VM/container hosted by a Mac is Linux evidence only. An arm64 cross-build is not a native process-lifecycle test. Installing GNU sed/new Bash to make failures disappear is not a compatibility fix; exercise `/bin/bash` and BSD tools explicitly.

## 2. Native isolation is more than HOME

Mac has no Linux Bubblewrap PID/network namespace equivalent in this plan. Private HOME/XDG is useful but does **not** prevent a script from calling absolute `launchctl`, opening a real daemon socket, reading the process table or sending host signals.

- Editor/pure parsing/static checks may run in the fresh Mac workspace after reviewing their effects.
- Before process, service-adjacent, package-launcher, broad-shell or browser lifecycle tests, use a **disposable macOS VM/runner with no production Herdr/Tailscale installation, credentials or mounted developer home**. A dedicated expendable Mac is an alternative if explicitly designated for this use.
- F01 must inventory helpers and enforce test-owned adapters, including sibling absolute paths. Do not execute `make check`, `tests/test_common.sh`, or unknown shell fixtures on a personal Mac merely because HOME points to `/tmp`.
- Provision toolchain/modules in a separate setup phase. During fixtures disable external network where possible, keep local loopback functional, and record the boundary. No developer tailnet connection or system-daemon use.
- Create one guest snapshot after provisioning; revert/dispose between destructive lifecycle scenarios if cleanup is uncertain. VM disposal must not become a product cleanup pass: failed teardown is still a failed test.

## 3. Toolchain checklist (record, do not silently install)

From the pinned source, check README/QUICKSTART/AGENTS/Makefile/go.mod/frontend/package.json again. Initial versions:

- Go **1.27.0** (`go.mod`), local toolchain only for verification.
- Bun **1.4.0**; frontend lockfile unchanged.
- Python 3 for fixture tooling; record exact version. It is not a production runtime dependency.
- Stock `/bin/bash` (typically 3.2), `/usr/bin/sed`, `/usr/bin/awk`, BSD `sort`, `shasum`.
- Xcode Command Line Tools / `clang` for Go race builds; Git, Make, ripgrep (`rg`, required by production-path-audit).
- Installed ShellCheck/actionlint versions recorded; do not erase pre-existing warnings by broad suppression. New diagnostics fail; a baseline must bind paths/messages and be honest.
- Declared Playwright **1.62.1** browser build for browser features; install/cache explicitly, not by changing package versions.
- Tailscale is **not required or invoked** until the separately authorized F12 run. Pinned real target remains 1.102.4/upstream `bbcd7d1fc…` unless F04 explicitly revises the contract.

Useful read-only inventory commands:

```bash
sw_vers
uname -m
/bin/bash --version
/usr/bin/sed -n '1p' /dev/null
command -v go bun python3 git make rg shellcheck actionlint
GOTOOLCHAIN=local go version
bun --version
python3 --version
xcode-select -p
```

If versions are unavailable, record a tooling blocker and arrange explicit provisioning. Do not silently fetch another Go toolchain or change `go.mod` to fit the host. A missing later browser/runner need not block source-only review of an unrelated feature.

## 4. Private local fixture profile

The runner added in F01 should create a short private path (Unix socket limits are small on macOS), for example `/tmp/herdr-ts.XXXXXX`, with 0700 directories. Under it keep HOME, XDG config/data/cache/state/runtime, TMPDIR, Python bytecode, fixture config, sockets and logs. Build binaries outside source; cache modules under a separate controlled workspace directory and use a private build cache.

Start subprocess fixtures with an explicit clean environment, not `os.environ.copy()` inherited from the developer shell. Pass only the required PATH, locale, toolchain, fixture endpoints and explicit test flags. Avoid inherited `HERDR_*`, `TS_*`, `GH_*`, Cloudflare credentials, SSH agents and deployment settings. Stub tools are first in PATH; verify absolute/sibling invocations separately.

Verification configuration:

```text
GOTOOLCHAIN=local
GOPROXY=off
GOSUMDB=off
GOFLAGS=-mod=readonly
GOMODCACHE=<provisioned test module cache>
GOCACHE=<private writable build cache>
PYTHONPYCACHEPREFIX=<private external path>
LC_ALL=C
```

`python -B -m py_compile` can still create `.pyc`; set the prefix. Do not rely on global umask to satisfy tests: fixtures needing 0600/0700 or 0644 must set those modes explicitly. F01.4 repairs the known conflicting umask assumptions. No broad-suite pass claim until the same declared environment actually passes.

## 5. Concrete local failure-first workflow

After F01's safety review, **inside the disposable native runner**:

1. Reproduce F02's old JSON boolean extraction with `/bin/bash`, a literal JSON fixture and `/usr/bin/sed`. No relay process necessary. Add malformed/string/lookalike controls first.
2. Run the exact new parser unit/regression only, then safe common-shell tests. Inspect stdout and exit status; no missing field defaults to true.
3. Build the relay supervisor once from current source into the private run directory. Run a single Darwin lifecycle case, diagnose, edit and repeat; do not push just to see a startup error.
4. Run remaining cases separately, then all four without filtering. Count outcomes; require foreign survival and owned EOF/retained evidence, not merely launcher exit.
5. Run relevant Go/race tests and the Linux counterpart before committing the feature checkpoint.

Existing native driver invocation (not a general developer-host command):

```bash
# All variables below must be supplied by F01's reviewed private runner.
# $RUN is private and short; $REPO is the sole source checkout; cwd is $REPO.
go build -trimpath -o "$RUN/herdr-mobile-relay" ./cmd/herdr-mobile-relay
HERDR_S9B3_DISPOSABLE_DARWIN=1 \
HERDR_S9B3_SOURCE="$REPO" \
HERDR_S9B3_SUPERVISOR="$RUN/herdr-mobile-relay" \
python3 -u tests/test_tailscale_process_groups_darwin.py -v \
  DarwinLifecycle.test_launcher_termination_retires_owned_roles_and_preserves_foreign

# Repeat without a unittest filter for the required four-case gate.
```

`HERDR_S9B3_DISPOSABLE_DARWIN=1` is an acknowledgement after establishing disposability, **not the thing that creates isolation**. Do not override platform checks on Linux or Intel to claim arm64 native success.

### Better local diagnostics, not more blind retries

F03.1 should detect a dead launcher while waiting for handshakes and report a bounded sanitized category immediately. Handshakes must cap bytes, time and duplicate roles; role sockets/EOF describe the test-owned lifetime. Keep fixture logs locally private, redact before sharing. Never dump the whole environment, setup fragment, device store, browser profile, CA private key or real CLI credential state into CI artifacts.

Set wall-clock limits with an owned supervisor/test harness, not GNU `timeout` assumed available on macOS. Do not add `pkill`, `killall`, `ps|grep|kill`, stored-PID signals or unpinned group cleanup. If a timed-out fixture cannot prove safe process ownership, mark failure and dispose of its dedicated guest rather than hunt host processes.

## 6. Linux regression from the Mac

Prefer a disposable Linux VM with working loopback and user namespaces, then run the existing Bubblewrap-contained tests inside it. Docker/Podman can be used only if their actual kernel/namespace permissions meet the tests; never convert an unavailable Bubblewrap capability into a skip/pass. Do not add a privileged host-container configuration just to suppress the error.

- Mount/copy the **same source snapshot** read-only for verification.
- Use cached modules offline, writable run-local caches and inert adapters.
- Record `uname`, architecture, source digest and namespace boundary.
- Run Linux-only `/proc` tests only here; run the common portable suite on both systems.
- If Linux execution is unavailable locally, the feature can be source-reviewed/Mac-verified with Linux pending. Its Linux-dependent acceptance and final readiness remain blocked until a matching-SHA Linux CI job succeeds.

## 7. CI promotion and final freeze

Do not use the old branch-only workflow as the permanent matrix: its trigger matches only `ci/tailscale-native-preflight-ef9f843`. After a locally green checkpoint, review the minimal trigger/job change for the new separate branch, then commit/push that branch only. No PR is necessary just to obtain a runner; no tag/release/deploy dispatch.

For each run bind actual commit, clean source tree, tool versions, archive digest and browser asset identity. Keep an immutable copy of the workflow/test inputs in evidence; do not manifest a moving checkout path and later claim that historical manifest still validates live. A diagnostic `continue-on-error` step requires an unavoidable final failure gate; final required test jobs should have ordinary failure semantics.

The Mac is the primary implementation/debugging machine. CI is an independent verifier, and a separately authorized real tailnet is a compatibility environment—not a shortcut around local failures or source review.
