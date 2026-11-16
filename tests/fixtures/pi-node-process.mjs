import { spawn } from 'node:child_process';

export const nodeTestTimeout = 60_000;
const deadlineMs = 30_000;

export function spawnNode(args, options = {}) {
  const child = spawn('node', args, { ...options, stdio: ['ignore', 'pipe', 'pipe'] });
  const output = { stdout: '', stderr: '' };
  child.stdout.on('data', data => { output.stdout += data; });
  child.stderr.on('data', data => { output.stderr += data; });
  const exited = new Promise(resolve => {
    child.once('error', error => { output.stderr += error.message; resolve({ code: -1, signal: null }); });
    child.once('close', (code, signal) => resolve({ code, signal }));
  });
  const report = exit => `exit code ${exit ? exit.code : 'none'}, signal ${exit ? exit.signal : 'none'}\nstdout: ${output.stdout}\nstderr: ${output.stderr}`;
  const until = async (work, what) => {
    let timer;
    try {
      return await Promise.race([
        work,
        exited.then(exit => { throw new Error(`Node child exited before ${what}: ${report(exit)}`); }),
        new Promise((_resolve, reject) => { timer = setTimeout(() => reject(new Error(`Node child did not ${what} within ${deadlineMs} ms: ${report()}`)), deadlineMs); }),
      ]);
    } finally { clearTimeout(timer); }
  };
  return {
    child,
    output,
    waitForStdout: text => until(new Promise(resolve => {
      const check = () => { if (output.stdout.includes(text)) { child.stdout.off('data', check); resolve(); } };
      child.stdout.on('data', check);
      check();
    }), `printing ${JSON.stringify(text)}`),
    finish: async () => {
      let timer;
      try {
        return await Promise.race([
          exited,
          new Promise((_resolve, reject) => { timer = setTimeout(() => reject(new Error(`Node child did not exit within ${deadlineMs} ms: ${report()}`)), deadlineMs); }),
        ]);
      } finally { clearTimeout(timer); }
    },
    stop: async () => {
      child.kill('SIGTERM');
      const forced = setTimeout(() => child.kill('SIGKILL'), 5000);
      try { return await exited; } finally { clearTimeout(forced); }
    },
  };
}
