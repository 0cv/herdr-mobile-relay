import { startBridge, normalizeCommands } from '../../relay/pi-command-bridge/bridge.mjs';
const [instance, pane, session, directory] = process.argv.slice(2);
const close = await startBridge({ instance, pane, session, directory, catalog: () => normalizeCommands([
  { name: 'orchestrate', description: 'Orchestrate fixture', source: 'extension', sourceInfo: { path: '/fixture/extension.ts', source: 'local', scope: 'project', origin: 'top-level' } },
], new Set()) });
const timer = setInterval(() => {}, 1000);
process.on('SIGTERM', async () => { clearInterval(timer); await close(); process.exit(0); });
console.log('ready');
