# Plan-only validation report

## Delivered

- 14 feature areas / 62 named substeps, each with an independent acceptance check.
- Receiving-Mac source import, toolchain/isolation/runbook, local-native debugging and Linux regression workflow.
- Separate session-authority design decision and real-tailnet authorization gates.
- Crosswalk covering PR-TS-001–018, TS-TEST-001 and newer parser/provenance/ownership limitations.
- Machine-readable initial status and a per-step execution/review template.
- Exact published-source inventory plus explicitly unapproved two-file patch, with read-only import utility.

## Checks performed here

Detailed results: `handoff/plan-checks.json`.

1. Confirmed all 62 table substeps occur exactly once in status and have valid dependencies; dependency graph is acyclic.
2. Checked local Markdown links and concrete existing command/test names against source.
3. Parsed the import utility as Python, with no bytecode generated in product checkouts.
4. In a **temporary standalone clone**, verified baseline HEAD, unchanged index, 826 source paths/modes/hashes and workflow identity.
5. Applied the unapproved patch in that temporary clone and verified it exactly reproduces candidate source hashes; it fails baseline mode, as intended.
6. Negative controls rejected altered source contents, extra source, changed mode, changed index, shared object alternates and an ignored inert fixture file. These checks exercised the final utility bytes, not the product.
7. Rechecked the real candidate's 826-path source inventory and the comparison original's 778-path inventory: paths/types/modes/hashes match their expected inventories. The only comparison membership exclusions are documented volatile `relay/relay.pid` and `relay/device-auth/devices.json`. No production `.env` was read or copied.
8. Final handoff integrity/archive checks are performed after writing this report; `SHA256SUMS` covers the report and all delivered inputs, not mutable external paths.

Temporary clones used by utility checks were removed. No candidate/original source edits, product builds/tests, native runs, real Tailscale commands, workflow launches, commits, pushes, PRs or deployments were performed during this planning task.

## Limits

This is an authored plan with structural/transfer checks, **not an independently approved orchestration plan or implementation**. Historical source/native results are identified as historical. The pending parser patch remains unapproved; Mac lifecycle and shared Serve authority remain unresolved. None of these plan-only checks changes `prReady=false`.

The final archive is a transfer convenience. Its checksums prove consistency with the received handoff manifest, not trust in the sender or correctness of the implementation. Use the pinned Git commit, source inventory, independent feature reviews and actual Mac/Linux/runtime gates during execution.
