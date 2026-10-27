if (!process.env.PI_LIVE_PTY_ARGS || !process.env.PI_LIVE_TEST_WORK) throw new Error('Explicit private PTY arguments required');
const terminal = new Bun.Terminal({ cols: 100, rows: 40, data(_terminal, bytes) { process.stdout.write(bytes); } });
const child = Bun.spawn(JSON.parse(process.env.PI_LIVE_PTY_ARGS), { cwd: process.cwd(), env: process.env, terminal });
process.stdin.on('data', bytes => terminal.write(bytes));
process.on('SIGTERM', () => child.kill('SIGTERM'));
process.on('SIGINT', () => child.kill('SIGTERM'));
const code = await child.exited;
terminal.close();
process.exit(code);
