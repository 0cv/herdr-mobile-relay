import { discoverTmuxServer, removeStaleTmuxSocket, verifyCurrentTmuxProof } from "./pi-scroll-qualification-tmux-proof.mjs";

const action = process.argv[2];
const input = await Bun.stdin.json();

try {
  let result;
  switch (action) {
    case "discover":
      result = await discoverTmuxServer(input);
      break;
    case "verify":
      result = await verifyCurrentTmuxProof(input);
      break;
    case "remove-stale":
      result = await removeStaleTmuxSocket(input);
      break;
    default:
      throw new Error(`unknown tmux proof action: ${action}`);
  }
  process.stdout.write(`${JSON.stringify(result)}\n`);
} catch (error) {
  process.stderr.write(`${JSON.stringify({ code: error.code ?? "tmux_proof_error", message: error.message })}\n`);
  process.exitCode = 1;
}
