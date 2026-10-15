import {
  allocateAttempt,
  claimStageLock,
  initializeStage,
  markRootCreated,
  recordCleanupRecovery,
  recordAttemptOutcome,
  releaseStageLock,
  stageSummary,
} from "./pi-scroll-qualification-ledger.mjs";

const action = process.argv[2];
const stageRoot = process.argv[3];
const input = await Bun.stdin.json();

try {
  let result;
  switch (action) {
    case "initialize":
      result = await initializeStage(stageRoot, input);
      break;
    case "cleanup-recovery-preview":
      result = await recordCleanupRecovery(stageRoot, { ...input, dryRun: true });
      break;
    case "cleanup-recovery":
      result = await recordCleanupRecovery(stageRoot, input);
      break;
    case "lock":
      result = await claimStageLock(stageRoot, input);
      break;
    case "allocate":
      result = await allocateAttempt(stageRoot, input);
      break;
    case "root-created":
      result = await markRootCreated(stageRoot, input);
      break;
    case "outcome":
      result = await recordAttemptOutcome(stageRoot, input);
      break;
    case "summary":
      result = await stageSummary(stageRoot, input);
      break;
    case "release":
      result = await releaseStageLock(stageRoot, input);
      break;
    default:
      throw new Error(`unknown ledger action: ${action}`);
  }
  process.stdout.write(`${JSON.stringify(result)}\n`);
} catch (error) {
  process.stderr.write(`${JSON.stringify({ code: error.code ?? "ledger_error", message: error.message })}\n`);
  process.exitCode = 1;
}
