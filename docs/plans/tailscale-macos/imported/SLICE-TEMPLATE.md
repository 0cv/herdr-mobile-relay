# <Fxx.y> — <one independently testable outcome>

## Input and authority

- Source checkout (absolute **Mac** path):
- Controller cwd (must match intended checkout):
- Branch / HEAD / complete input source digest:
- Dirty paths expected at entry (prefer none):
- Required prior feature outcomes:
- Historical limitation carried forward, if any:
- Execution class: source-only / pure-file / native-fixture / package / Layer 1 / separately authorized Layer 2:
- Isolation/capability proof (not merely an environment flag):
- Editable files (explicit):
- Read-only inputs / forbidden paths / forbidden operations:
- New dependency or authority required? If yes, stop for that decision before implementation.

## Contract and scope

One sentence describing the invariant. State positive behavior, refusal behavior, preservation, relevant compatibility and time/output bounds. Include what this slice explicitly does **not** qualify.

Do not label unrelated findings as fixed in this slice. If more files/callers become necessary, revise this allowlist before editing, or create a successor substep.

## Test-first sequence

1. Reproduce/fail the named assertion on baseline, safely.
2. Make the minimal source/test change.
3. Execute the focused command with private state and current source.
4. Execute named negative/fault/mutant controls; failure must occur at intended assertion.
5. Run the affected regression set on the specified platform(s).
6. Compare complete input/output inventory, HEAD/index/status and unchanged protected paths.

### Exact gates

| Gate | Command / case names | Platform/environment | Expected evidence | Failure disposition |
|---|---|---|---|---|
| Focused positive | | | | |
| Negative/control | | | | |
| Related regressions | | | | |
| Static/build | | | | |
| Scope/identity | | | | |

Case counts alone are insufficient: list required cases and any explicitly optional existing tests. No required skip is a pass. One test-run setup failure is not a successful mutant kill.

## Handoff and independent review

- Worker records actions/evidence and returns `ready for review`; no self-dispatch of reviewers.
- Reviewer examines the **same absolute Mac checkout** and exact source digest, relevant callers, fixture safety and controller evidence; reviewer writes no repository files.
- Source-only reviewer must not demand shell execution tools or claim personal runs. Controller/designated verifier executes separately on frozen source.
- Distinguish later native/Linux/tailnet dependencies from gates promised in this slice; don't block a parser source review solely because real tailnet access is absent.
- Maximum three repair passes. If the contract/scope changes or repeated failures don't converge, stop, retain evidence and split/diagnose. Do not retry the same impossible prerequisite or patch the orchestration controller.
- If a review finds a source bug in a **read-only** closure task, end with that finding and create a bounded implementation task. Never let the repair turn edit the controller's unrelated cwd.

## Result

- Source review: pending / clean / findings / blocked.
- Focused tests:
- Native Mac / native Linux:
- Package / Layer 1 / Layer 2 (only when applicable):
- Final source/diff hash and reviewed commit:
- Failed attempts and their disposition:
- Unresolved findings / downstream invalidations:
- Public/sanitized evidence directory:
- Private raw evidence directory (never publish secrets):
- Commit/push allowed only to the separate feature branch after required checks; no PR/release.

Write the report and review outcome before finalizing an immutable evidence manifest. Update only this substep's record in `status.json`, then select the next dependency-ready substep. Approval is bounded to this named slice, never automatically global C2/N1/product approval.
