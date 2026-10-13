# Transfer to the Mac: source of truth and first commands

## What to transfer

Copy this **entire plan directory** (or its sibling `.tar.gz`) to the other Mac. It contains:

- `PLAN.md`: features F00–F13 and independently verifiable substeps.
- `MACOS-RUNBOOK.md`: native-local iteration and isolation model.
- `VERIFICATION.md`: test interfaces, exact existing commands and acceptance semantics.
- `ACCEPTANCE-MATRIX.md`: mapping to original findings and final gates.
- `SLICE-TEMPLATE.md`, `status.json`: per-step execution handoff/checkpoint.
- `handoff/provenance.json`, `handoff/source-inventory.json`: machine-readable source identity.
- `handoff/UNAPPROVED-portable-bool.patch`: **two unapproved local edits**, reference only.
- `verify-source.py`: read-only import verifier for the exact baseline or optional patch.
- `SHA256SUMS`: integrity manifest for this handoff, not a signature or source approval.

No production `.env`, devices, credentials, browser profiles, runtime directories, module caches or binaries are included. No Linux absolute workspace path is needed to execute the plan on the Mac. Old workflow result files are not a prerequisite; some historical controller paths are no longer available.

## Published baseline versus current uncommitted work

Use this exact, reproducible Git commit by default:

```text
repository: https://github.com/0cv/herdr-mobile-relay.git
fetch branch: ci/tailscale-native-preflight-ef9f843
baseline commit: b74a1c16c9c4a1b1207cad88fb00b33556e04025
original pre-feature base: ef9f84351b5f257f1e8e0156057198fe8e644dd3
source paths (excluding added CI workflow): 826
source-tree digest at CI: ff8ff3deb6722cc29d95fce660e66d4af92491509a1cf483e84ab7231fcf072b
added workflow: .github/workflows/tailscale-native-preflight.yml
workflow SHA-256: 2547b80f197432eed5156203e73e94d40609ae30485fb3d6be56a91028523884
```

This is a **red preflight snapshot**, not a release. Existing authentication, lock, transaction and supervision implementation is worth preserving; do not restart the feature from `dev`.

The current Linux candidate differs from that published source in exactly two files:

| File | Current local SHA-256 | Disposition |
|---|---|---|
| `relay/common.sh` | `d254d929ce99c7691b59819b69d61761794f181956bee77d1a3145c716e687ca` | Unapproved portable-bool change; escaped-string/field bug still open. |
| `tests/test_common.sh` | `b9da93650f37744774b40cfb85c2c8c6e92c26597e129bd842c49b8f1c23e072` | Companion regression/fixture changes; not independently approved. |

`source-inventory.json` records both `baseline_sha256` and current `sha256` for every source path. The patch was checked in a disposable directory to apply to the baseline and reproduce both current hashes. That proves transfer fidelity **only**, not correctness.

**Recommended:** start F02 from the published baseline and use the patch as a source of failing tests/diagnostic context. If applying it for investigation, do so only in an explicit scratch branch and label it unapproved; do not present its local test passes as review approval.

## Import on the receiving Mac

These are **future commands on the Mac**, not actions performed while authoring this plan. They fetch source only; they do not start a relay or Tailscale. Use a new, private directory and no existing product checkout. `PLAN_DIR` is wherever you copied this handoff.

```bash
export PLAN_DIR="$HOME/Downloads/tailscale-macos-plan"  # adjust to copied directory
cd "$PLAN_DIR"
shasum -a 256 -c SHA256SUMS

export WORK_ROOT="$HOME/Developer/herdr-tailscale-macos"
# Require a new root; stop and choose another name if it exists.
test ! -e "$WORK_ROOT" || { echo 'Choose a new WORK_ROOT'; exit 1; }
mkdir -m 700 -p "$WORK_ROOT"

git clone --single-branch --branch ci/tailscale-native-preflight-ef9f843 \
  https://github.com/0cv/herdr-mobile-relay.git "$WORK_ROOT/repo"
export REPO="$WORK_ROOT/repo"
export BASE=b74a1c16c9c4a1b1207cad88fb00b33556e04025

git -C "$REPO" cat-file -e "$BASE^{commit}"
git -C "$REPO" switch --detach "$BASE"
git -C "$REPO" switch -c "work/tailscale-macos-$(date +%Y%m%d-%H%M%S)"
python3 "$PLAN_DIR/verify-source.py" --root "$REPO" --mode baseline
```

If clone, commit availability, toolchain, modes or hashes fail, **stop import**. Do not fall back to a moving branch/HEAD, switch to another repository's `dev` remote, discard existing local work or loosen the verifier. Normal dependency/tool acquisition is a separate explicit setup step, not a silent side effect of tests.

Optional scratch investigation only:

```bash
# Only on a fresh scratch branch with the verified baseline and clean index.
git -C "$REPO" apply --check "$PLAN_DIR/handoff/UNAPPROVED-portable-bool.patch"
git -C "$REPO" apply "$PLAN_DIR/handoff/UNAPPROVED-portable-bool.patch"
python3 "$PLAN_DIR/verify-source.py" --root "$REPO" --mode candidate
```

Do not run these optional commands in the implementation checkout unless F02 deliberately chooses this starting point. Prefer a second standalone clone when preserving an untouched reference; no shared Git worktree/index with any original checkout.

## Historical evidence to understand, not blindly reuse

- [First native preflight](https://github.com/0cv/herdr-mobile-relay/actions/runs/35950305948), commit `54b1150595f72effd35b469627ff2a18ffc51e0a`: source provenance, scoped native Go tests/vet/build passed; 4/4 Darwin lifecycle cases failed waiting for role handshakes.
- [Diagnostic native preflight](https://github.com/0cv/herdr-mobile-relay/actions/runs/35952772727), baseline `b74a1c1…`: same native Go success and lifecycle failure. Bounded summary: four 70-byte launcher logs, zero child logs, fixed category `exposure_inspection_incomplete`. No raw fixture secrets uploaded. Artifact retention may have expired; the plan does not require downloading old artifacts to begin.
- GNU `sed --posix` reproduces the old accessor's failure on valid booleans; native BSD confirmation and an end-to-end native rerun still belong to F02/F03. Do not infer all remaining Mac issues are solved by replacing one regex.
- C2F1C stabilized a real Bash forced-KILL fixture: actual child retirement can coexist with safe proof refusal after job-record pruning. Never demand fake success, unconditional cache, or PID signaling to turn this green.
- Historical C2F2D full dirty-tree proof was missing. A new inventory cannot reconstruct past unknown content. Retain that limitation, qualify/review the current committed baseline afresh, and obtain an explicit final disposition rather than loop forever trying to find nonexistent evidence.
- Do not reuse edits or review assertions from the contaminated-original run `0ec3e7f2…`. The original's four affected files were restored to their exact prior hashes; none of those accidental repairs is in the candidate. The actual parser finding remains open.

## One writable source root — mandatory

Before **every** worker and verifier, assert canonical `git --show-toplevel`, branch, HEAD and expected dirty set. Hand agents the absolute **Mac** path, not this machine's path. The controller cwd must equal that checkout, or it must use an explicitly isolated workspace rooted there. A label in a prompt is not a filesystem write barrier.

Keep the imported reference and historical evidence read-only; where practical run workers in a disposable guest with only the intended checkout writable. No original developer checkout, production home or authenticated Tailscale daemon needs to be mounted. All source changes go through the same reviewed branch. Never repair a failed test by editing a different checkout, a release extraction, or only the CI copy.
