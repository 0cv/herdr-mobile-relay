# Start here — macOS execution handoff

**Main plan: [PLAN.md](PLAN.md).** It has **14 feature areas, split into 62 small, independently verifiable substeps**. The execution machine is a different Mac; this directory is self-contained and does not require old Linux workspace paths or workflow databases.

## Reading/execution order

1. [HANDOFF.md](HANDOFF.md): import pinned source, verify its identity, understand the unapproved two-file patch.
2. [MACOS-RUNBOOK.md](MACOS-RUNBOOK.md): stock macOS tools, disposable native fixtures, fast local debugging, Linux VM regression loop.
3. [PLAN.md](PLAN.md): start **F00.1**; take one numbered substep at a time.
4. [VERIFICATION.md](VERIFICATION.md) and [SLICE-TEMPLATE.md](SLICE-TEMPLATE.md): exact evidence and review contract.
5. [ACCEPTANCE-MATRIX.md](ACCEPTANCE-MATRIX.md): all original findings and final acceptance obligations; update [status.json](status.json) as work completes.

## Feature map

| ID | Feature |
|---|---|
| F00 | Reproducible Mac import and safe writable workspace |
| F01 | Fast, safe local tests and real platform coverage |
| F02 | Correct portable JSON/inspection consumption |
| F03 | Owned foreground processes and bounded cleanup |
| F04 | Independently bound real Tailscale session authority |
| F05 | Shared ownership across all configuration/install writers |
| F06 | Transactions, reprint, IPC and explicit recovery |
| F07 | Exact release/package identity and readiness |
| F08 | Setup menu, consent, resolver and legacy compatibility |
| F09 | Authentication/device preservation and safe updates |
| F10 | Exact-artifact trusted-TLS/browser harness |
| F11 | Real packaged-browser acceptance journeys |
| F12 | Separately authorized real two-node tailnet qualification |
| F13 | Documentation, CI and final exact-revision readiness |

First local milestone: safe Mac harness → parser → four native lifecycle cases. Start the **F04 authority decision early** in parallel with that work; it is a real cross-platform functional blocker, not just a testing inconvenience. Do not spend weeks completing peripheral work while assuming the current CLI can prove session ownership.

## What is and is not finished

The published snapshot has passing focused Linux fixtures and native Mac scoped Go tests/builds, but both native Mac launcher CI runs are red. Real browser/tailnet acceptance is unfinished on both platforms. The local-only parser patch is unapproved and still has a JSON correctness finding. This plan preserves these facts, rather than presenting the task as a Mac-only polish pass.

The plan was written and checked for internal structure/import fidelity; it is **not independently source-reviewed implementation**, and no product tests, tailnet actions or source edits were performed as part of this planning task. See [PLAN-VALIDATION.md](PLAN-VALIDATION.md) for the exact plan-only checks.

## Transfer and integrity

Transfer the entire directory or the sibling archive. Inside the unpacked directory run:

```bash
shasum -a 256 -c SHA256SUMS
```

Then follow HANDOFF. Checksums detect alteration; they do not replace trust in the handoff channel or Git/source review. `verify-source.py` is read-only and checks the 826 source paths against the exact pinned commit. Its optional candidate mode is for the unapproved patch, not acceptance.

Do not start on `main` or `dev`. Do not copy developer secrets/runtime state. Do not start real Tailscale or alter host trust/services during fixture work. A Mac disposable runner is a real isolation requirement, not just setting a test environment flag.
