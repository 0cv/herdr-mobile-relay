import { instanceIdentity } from '../../relay/pi-command-bridge/bridge.mjs';
const results = [];
for (const path of process.argv.slice(2)) {
  try { results.push({ instance: await instanceIdentity(path) }); }
  catch { results.push({ error: true }); }
}
console.log(JSON.stringify(results));
