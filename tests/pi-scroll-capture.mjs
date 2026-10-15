import { appendFileSync } from 'node:fs';
import { join } from 'node:path';

const root = process.env.HERDR_P1_FIXTURE_ROOT;
if (!root || process.env.PI_OFFLINE !== '1' || process.env.PI_CODING_AGENT_DIR !== join(root, 'pi')) throw new Error('isolated Pi output capture required');
const original = process.stdout.write;
let size = 0;
process.stdout.write = function(chunk, ...args) {
  const data = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk, typeof args[0] === 'string' ? args[0] : 'utf8');
  size += data.length;
  if (size > 1024 * 1024) throw new Error('Pi output capture bound exceeded');
  appendFileSync(join(root, 'logs', 'pi-output.raw'), data, { mode: 0o600 });
  return original.call(this, chunk, ...args);
};
