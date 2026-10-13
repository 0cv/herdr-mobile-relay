# Independent verification contract

## State model: do not collapse different kinds of success

Each substep records separate values for:

- `sourceReview`: pending / clean / findings / blocked.
- `focusedTests`: not_run / pass / fail / blocked.
- `nativeMac`, `nativeLinux`, `packagedRuntime`, `layer1`, `layer2`: not_applicable / not_run / pass / fail / blocked.
- Exact source revision/tree digest, changed paths, environment and commands.

A source-only reviewer reads code/gate definitions/controller evidence, and does not claim to have personally executed tests. The controller or designated verifier runs commands on frozen inputs. `not_applicable` requires a written reason; it cannot excuse a mandatory platform/feature gate. Historical approval carries over only to **unchanged inputs and unchanged relevant interactions**, not by reusing an old run ID.

A feature substep can be source-reviewed and locally tested while its later integration gate remains pending. Examples:

- F02 may pass portable parser tests without F12 credentials.
- F03 may pass native inert lifecycle without live session authority; it cannot then claim a real Serve route is owned.
- F04.1 may deliver a sound ADR/decision request; F04 is not implemented by that document.
- F13 cannot report ready while mandatory native/browser/tailnet gates or substantive findings are open.

## Proposed focused interfaces (implemented in F01; not present at import)

Create lightweight wrappers/Make targets rather than a second test framework. Reuse existing fixtures where safe. The wrappers must take explicit source/evidence roots, validate platform and runner capability, use private environment and exit nonzero on missing required prerequisites.

| Proposed command | Runs | Deliberately excludes |
|---|---|---|
| `make tailscale-unit` | Scoped config/parser/policy/journal/reprint/file-socket tests and chosen parser regression | Real CLI/daemon, external HTTP, service managers, native full lifecycle |
| `make tailscale-portable` | Safe common-shell, resolver/consent, readout, quoting and extracted-wrapper contracts under stock shell/utilities | `/proc`, GNU-only helpers, production sibling services |
| `make tailscale-macos CASE=<optional case>` | Native Go/supervisor + Darwin role/EOF lifecycle fixtures in disposable Mac runner | Real tailnet; Linux kernel claims |
| `make tailscale-linux CASE=<optional case>` | Existing private Bubblewrap child-job/launcher/group/supplemental process tests | Mac qualification; host process cleanup |
| `make tailscale-package TARGET=<os/arch>` | Exact archive/extraction identity and safe native smoke when target equals host | Release publication; running a cross-compiled executable as proof |
| `make tailscale-browser CASE=<optional case>` | Exact packaged browser Layer 1 with real TLS/WSS/relay/fake Herdr | Fake relay/WebSocket, injected credentials, real tailnet |
| `make tailscale-acceptance` | Mandatory final aggregate plus evidence validator | Silent skips, partial green or cross-revision artifacts |

`CASE` is a developer filter only. Required CI/acceptance runs must reject a filtered invocation or mark it incomplete, and verify the full expected case list. Do not introduce all wrappers at once: add the one needed for the current feature, with its refusal/self-tests.

## Existing project commands

Run **only after F01 safety/portability work**, in the reviewed disposable environment. These commands exist now; whether they pass is not asserted here:

```bash
# Read-only formatting check; require no filenames in output.
gofmt -l cmd internal tests

go vet ./...
go test ./...
go test -race -p 1 ./...

make shell-check production-path-audit
make cross-build
make release-bundle-check

# Explicit setup step for the pinned lockfile, before offline verification:
bun install --frozen-lockfile --cwd frontend
# Existing commands: lint, type check, unit tests, build.
make frontend-check
make web-release-check

# Final project aggregate, not the inner debugging loop:
make check
```

Cache preparation/downloads are separate from offline tests. Existing `make check` may cover overlapping work; don't rerun full frontend/browser/race gates on every parser edit. Use focused tests locally, the feature regression set before its checkpoint, and a single declared full matrix for final freeze. No `make dev-tunnel`, `web-deploy`, `relay-plugin`, live service/install or production Herdr commands as testing shortcuts.

`tests/test_tailscale_process_groups_darwin.py` is currently Darwin/arm64-gated. `tests/test_tailscale_child_jobs.py` and Linux groups have private-namespace guards. Do not fake those guards to run in the wrong environment. Preserve intentional opt-in unrelated tests (e.g. journald) with explicit capability/disposition; required Tailscale tests must never be counted as passed when skipped.

## Required regression families

Minimum original cases are a reference, not a forever-fixed count. Add new assertions/cases without deleting existing obligations. For any split suite, preserve a source-to-new-case mapping.

| Family | Existing starting points | Mandatory assertions |
|---|---|---|
| Config/auth/reset | `internal/config`, `internal/transport`, `internal/deviceauth`, managed app tests | Exact key, all parsed true reset spellings refused, keyed auth, enrolled-store preservation |
| CLI/readout | `internal/tailscale/contract_test.go`, `supervision_test.go`, common-shell tests | Exact version/schema; incomplete/unknown never empty; escaped keys/strings/duplicates; bounded pipes/output |
| Shell child generation | `tests/test_tailscale_child_jobs.py` (19 at handoff) | Captured jobspec+PID, stopped/terminal/pruned transitions, no PID signal, post-KILL proof/cache discipline, foreign survival |
| Launcher ordering | `tests/test_tailscale_launcher_order.py` (7 at handoff) | No pre-admission write, successful rollback versus explicit proof-refusal retention |
| Process groups | Linux and Darwin group drivers (4 each at handoff) | Leader exits, descendants, controlled shutdown, retained failure evidence, foreign survival |
| Shell preflight | `tests/test_tailscale.sh` (16 supplemental cases at candidate history) | Complete/incomplete inspection distinct; inert side effects; no preexisting route changes |
| O/T/journal/IPC | `internal/managedstate`, `internal/localcontrol`, managed commands | Cross-writer contention, bounded T, identity-checked retirement, exact rollback and explicit recovery |
| Mode/menu/update | consent/pre-guard/owner/reprint/release-identity/space-path tests; config/update packages | Invocation-local consent, resolver parity, stale policy refusal before effects, app selection preserved |
| Package | release scripts/inventory + extracted smoke | All wrapper resources and web; archive/manifest/binary identity; no mutable-current mixing |
| Real frontend | dedicated Layer 1 suite using shipped web | Generated link, real auth/two devices/command, worker, restart, interruptions and refusal matrix |
| Real Tailscale | dedicated Layer 2 | Supported actual binaries, distinct remote node, independently bound session, foreign preservation |

## Negative controls and stress without chasing green

For each safety fix use one focused unsafe variant or fault injection in a **disposable copy**: missing completeness admitted, duplicate key accepted, stale owner trusted, wait/cache proof removed, unconditional unlink, stale updater permitted, missing readiness accepted, archive substituted, TLS untrusted, invitation arm acknowledgement lost. Require failure at the named safety assertion—not syntax, missing tool or an unrelated setup exception. Never mutate the main source just to run a mutant.

Race/stress runs have a fixed count and time bound chosen before execution. Record all outcomes; one intermittent failure blocks the affected gate until diagnosed. No increasing sleeps until green, switching umasks per failing test as the permanent solution, rerunning until a lucky pass, or asserting success after a proof refusal. Legitimate fail-closed outcomes require their own explicit no-effect/no-cache/retained-evidence assertions.

## Exact bytes and evidence discipline

Per accepted substep, create a unique directory **outside the source checkout**:

```text
<evidence>/<feature>/<step>/<run-id>/
  inputs.json                  source commit/tree/index + tool/platform identity
  source-inventory.json        complete declared source paths/modes/hashes
  commands.json                exact argv, safe env names, exit/time/case counts
  logs/                        sanitized logs; private raw data stored separately
  diff.patch                   allowlisted source changes
  report.md                    outcome, limitations, failed attempts, cleanup
  review.json                  independent review of named source/gates
  SHA256SUMS                   written after reports; never overwritten
```

Snapshot every manifest-covered input into immutable evidence or identify an immutable Git blob; do not hash a mutable checkout path into a supposedly permanent report. Hash all declared source paths, including expanded untracked files, using an unambiguous serialization and a fixed ignored-file policy. Preserve literal symlink target/mode/type; reject escapes/special files. At import no ignored files are expected; later separate known build caches from source explicitly—never exclude a changed source path after a failure.

Compare before/after full inventory + HEAD/index/status. Status alone does not detect changes inside an already modified file. New test/fixture files must enter the inventory. Bind review and execution to the same inputs; an edited shared helper invalidates relevant callers' prior evidence. Re-run the affected dependency cone, not every unrelated historical task.

Reports should state source, native Linux, native Mac, package, Layer 1 and Layer 2 results individually. Missing evidence is missing, not a zero failure count. Signing/authenticity of transferred artifacts is separate from a checksum; compare against the agreed source commit and trusted handoff channel.

## Acceptance on the final source revision

Required distinct cells:

1. Source review and complete current inventory; all substantive findings dispositioned.
2. Native Linux portable/lifecycle + package behavior.
3. Native macOS stock-shell/lifecycle + package behavior; record architecture.
4. Complete build/static/unit/race/frontend regression set.
5. Exact packaged real-browser Layer 1 and evidence negative tests.
6. Separately authorized real two-node Layer 2 for supported distributions.
7. Stopped manual-update preservation, docs and no unauthorized external effects.

If project support includes Linux arm64 or macOS Intel, distinguish compile coverage from actual native package/lifecycle coverage. Use native CI/authorized runners where the receiving Mac cannot provide it; if unavailable, keep that support cell pending rather than silently narrowing the advertised support matrix.
