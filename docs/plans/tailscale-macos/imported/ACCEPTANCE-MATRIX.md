# Coverage map and release acceptance

Historical work is implementation/reference evidence to preserve, not a promise that today's bytes satisfy every finding. The receiving implementer must use this table so a smaller feature slice never loses a larger requirement.

## Original findings → feature gates

| Finding | Obligation | Existing work to retain | Completion features |
|---|---|---|---|
| PR-TS-001 | Tailscale needs exact relay key and keyed auth | Config/auth guard and tests | F09.1, F11, F12 |
| PR-TS-002 | No bootstrap reset spelling can erase devices | Parsed `RearmBootstrap` guard/preservation tests | F09.1–.2, F11.3 |
| PR-TS-003 | Unknown/malformed exposure never means empty | Pinned typed parser/completeness | F02, F04, F11/F12 refusals |
| PR-TS-004 | One owner; competitors cannot overwrite/delete state | Managed-state O/T helpers, holder/start wiring | F05, F06, F03 |
| PR-TS-005 | Fresh invocation-local exposure consent | `--confirm-serve` and TTY behavior | F08.3, actual F11 menu journey |
| PR-TS-006 | Stale updater eligibility never authorizes effects | Manager/worker policy guards | F05.7, F09.3 |
| PR-TS-007 | Revalidate node/auth/account/tailnet/origin before effects and while running | Typed identity metadata | F04.4, F08, F11/F12 identity drift |
| PR-TS-008 | Logged-out/incomplete inspection retains evidence | Completeness checks | F02, F03, F06 |
| PR-TS-009 | Exact HTTPS route tuple AND independently bound session | `ExactRouteMatch`, intentional ownership refusal | F04, F12; route observation alone cannot close |
| PR-TS-010 | Socket error/close paths preserve foreign replacement | Auto-unlink disabled, identity checks | F06.3, DR-2 decision in F05.1 |
| PR-TS-011 | Durable exact-byte transactions and recovery | Journals, T-only reprint, stopped recovery | F06, all writer admission F05 |
| PR-TS-012 | Usable, coherent, physically pinned packaged release before pairing | Binary/public identity helper | F07, F10, F11 |
| PR-TS-013 | Actual managed UDP bind failure is fatal | Managed UDP fatal/legacy compatibility branch | F07.3, F11 refusals |
| PR-TS-014 | Bounded pipes and generation-safe lifecycle on both OSes | Go anchor/supervisor, Bash jobspec/cache, fixture fixes | F01, F03, final native cells |
| PR-TS-015 | Shell/Go resolver parity; legacy modes still work | Partial resolver and guard work | F08, F05 admission, F09 policy |
| PR-TS-016 | Binary paths with spaces remain single arguments | Quoted launcher invocations | F02/F03 regressions, F07.2/.6 |
| PR-TS-017 | Extracted setup has all wrappers/resources | Speech helper and `.env.example` packaging | F07.5/.6 |
| PR-TS-018 | Supported macOS has real portable/native test coverage | Darwin test driver, still unqualified | F01.3, F03, F13.2 |
| TS-TEST-001 | Hostname tests cannot reach real sibling service | Copied sibling adapter topology | F01.1, keep before broad suites |

## Additional open items discovered during closure

| Item | Do not confuse it with | Required handling |
|---|---|---|
| DR-1: parent-bound Serve session authority absent | Child PID/port/new-route observation | Early ADR + explicit scope decision F04, actual positive F12 evidence |
| DR-2: portable check/unlink is not atomic against arbitrary same-UID replacement | `os.SameFile` or `rename-noreplace` as a complete adversarial guarantee | Honest threat model and owner decision F05.1, native tests F06.3 |
| JSON escaped-string field confusion in local-only patch | A BSD-compatible regex as correct JSON parsing | Correct bounded structured consumption + targeted regressions F02 |
| C2F2D missing historical all-file baseline | Present inventory or unchanged git status proving past dirty bytes | Preserve historical blocker, fresh baseline/current full review F00.3; explicit disposition F13.3 |
| Test umask/loopback assumptions | Passing selected packages under favorable environments as full-suite pass | Explicit fixture modes and private loopback contract F01.4 |
| NVM/BSD utility suspicion | Unverified reviewer claim automatically requiring a product rewrite | Reproduce natively first, isolated portability substep if necessary F01 |
| Previous wrong-worktree worker edits | Candidate fixes or valid evidence for candidate | Original restored; use fresh isolated Mac workspace and correct controller cwd F00 |

## User-visible milestone labels

- **M0 — Mac-ready development:** F00 and safe runner prerequisites. Not feature working.
- **M1 — Native fixture lifecycle:** F02/F03 pass on Mac and Linux, no real tailnet. Not live ownership or phone pairing.
- **M2 — Source safety/core integration:** F04's authorized adapter + F05–F09 clean, current native/package gates. Not browser/tailnet acceptance.
- **M3 — Packaged browser accepted:** all F10/F11 cases and same-artifact evidence pass. Still synthetic Tailscale control plane, real data plane.
- **M4 — Real tailnet accepted:** separately authorized F12 passes on required supported runtime cells.
- **M5 — Ready to propose PR:** F13 and all substantive findings/current gates complete, no missing required evidence. PR/release publication still separately authorized.

Never use “works fully on Linux/macOS” at M0/M1. At intermediate milestones state exactly which packages/fixtures/runtime passed and which remain blocked.

## No shortcut substitutions

| Required proof | Does not substitute |
|---|---|
| Native Darwin lifecycle | Linux tests, `GOOS=darwin` build, Darwin `uname` stub |
| Native Intel support | arm64 run, cross-build or Rosetta assertion without a native target cell |
| Owned Serve session | Matching URL/backend, PID, synthetic `serve_route_owned=true` |
| Device persistence | Successful arm status alone, injected localStorage/credential |
| Exact package | Running checkout scripts with an extracted binary |
| Trusted TLS/worker | `ignoreHTTPSErrors`, insecure flags, host trust change, mocked SW |
| Remote tailnet path | Two processes on one loopback host or a proxy called “Tailscale” |
| Full source preservation | HEAD/index/status or curated hashes of only touched paths |
| Final acceptance | Historical green checks from different commits, hidden skips or a lucky rerun |

## Final readiness record

The final reviewer should complete a compact result that names:

- Source commit and full tree inventory digest.
- Exact release archive/checksum/manifest and target platform identities.
- Native Mac/Linux and any other advertised target outcomes.
- Normal/race/static/frontend/package suite outcomes.
- Layer 1 browser cases, exact web/service-worker identity and TLS negatives.
- Layer 2 distinct nodes, supported CLI/daemon versions, ownership proof and cleanup outcome.
- Every finding and decision request's current disposition, including historical scope limitation.
- Stopped manual update/device/app-origin preservation and documentation review.
- Explicit absence of production mutations, no main/dev pushes, no unapproved publication.

Unknown or failed mandatory fields keep `prReady=false`. Final certainty is bounded by the declared tests and threat model; do not promise absence of all possible defects.
