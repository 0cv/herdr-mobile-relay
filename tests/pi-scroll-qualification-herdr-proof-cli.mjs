import { discoverHerdrServer, verifyCurrentHerdrProof, verifyHerdrStopped } from "./pi-scroll-qualification-herdr-proof.mjs";

const action = process.argv[2];
const input = await Bun.stdin.json();

try {
  let result;
  switch (action) {
    case "discover":
      result = await discoverHerdrServer(input);
      break;
    case "verify":
      result = await verifyCurrentHerdrProof(input);
      break;
    case "stopped":
      result = await verifyHerdrStopped(input);
      break;
    default:
      throw new Error(`unknown Herdr proof action: ${action}`);
  }
  process.stdout.write(`${JSON.stringify(result)}\n`);
} catch (error) {
  process.stderr.write(`${JSON.stringify({ code: error.code ?? "herdr_proof_error", message: error.message })}\n`);
  process.exitCode = 1;
}
