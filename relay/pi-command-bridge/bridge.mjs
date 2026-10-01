import { createServer } from 'node:net';
import { lstat, mkdir, realpath, chmod } from 'node:fs/promises';
import { createHash, randomUUID } from 'node:crypto';
import { join } from 'node:path';

const maxBytes = 2 * 1024 * 1024;
const maxEntries = 4096;
const ownerKey = Symbol.for('herdr.mobile.commandBridge');
const probeKey = Symbol.for('herdr.mobile.commandBridge.autocomplete');
const text = (value, limit) => typeof value === 'string' ? value.replace(/[\x00-\x1f\x7f]/g, ' ').slice(0, limit) : '';

export function sanitizeSource(source) {
  source = source.trim();
  const prefix = source.startsWith('git:') && !source.startsWith('git://') ? 'git:' : '';
  let value = source.slice(prefix.length).trim();
  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(value)) {
    try {
      const url = new URL(value);
      url.username = '';
      url.password = '';
      url.search = '';
      url.hash = '';
      return prefix + url.href;
    } catch { return 'redacted'; }
  }
  if (value.includes('://')) return 'redacted';
  if (prefix || value.startsWith('git@')) {
    const end = value.indexOf('/');
    const authority = end < 0 ? value : value.slice(0, end);
    try {
      const decoded = decodeURIComponent(authority);
      const at = decoded.lastIndexOf('@');
      if (at > 0) value = decoded.slice(at + 1) + (end < 0 ? '' : value.slice(end));
    } catch { return 'redacted'; }
  }
  return (prefix + value).split(/[?#]/, 1)[0];
}

export function sanitizePath(path) {
  return path.replace(/[^/\\]+/g, part => {
    let decoded;
    try { decoded = decodeURIComponent(part); }
    catch { return 'redacted'; }
    const at = decoded.lastIndexOf('@');
    if (at > 0) return decoded.slice(at + 1);
    return /[?#]/.test(decoded) ? decoded : part;
  }).split(/[?#]/, 1)[0];
}

function abortable(work, signal) {
  return new Promise((resolve, reject) => {
    const aborted = () => reject(signal.reason || new Error('Discovery cancelled'));
    if (signal.aborted) aborted();
    else signal.addEventListener('abort', aborted, { once: true });
    Promise.resolve(work).then(resolve, reject).finally(() => signal.removeEventListener('abort', aborted));
  });
}

export function advertisedSkillNames(commands, suggestions) {
  const counts = new Map();
  for (const item of (suggestions?.items || []).slice(0, 16384)) {
    const name = item.value.replace(/^\//, '');
    counts.set(name, (counts.get(name) || 0) + 1);
  }
  for (const command of commands.slice(0, 16384)) {
    if (command.source !== 'skill') counts.set(command.name, (counts.get(command.name) || 0) - 1);
  }
  return new Set([...counts].filter(([, count]) => count > 0).map(([name]) => name));
}

export function createCatalog(getCommands, getProvider) {
  return async signal => {
    const provider = getProvider();
    if (!provider) return { commands: [], status: 'loading', truncated: false };
    const partial = () => ({ ...normalizeCommands(getCommands(), new Set()), status: 'partial' });
    if (globalThis[probeKey] || signal.aborted) return partial();
    const work = Promise.resolve().then(() => {
      signal.throwIfAborted();
      return provider.getSuggestions(['/skill:'], 0, 7, { force: false, signal });
    });
    globalThis[probeKey] = work;
    const settled = () => { if (globalThis[probeKey] === work) delete globalThis[probeKey]; };
    work.then(settled, settled);
    try {
      const suggestions = await abortable(work, signal);
      const commands = getCommands();
      return normalizeCommands(commands, advertisedSkillNames(commands, suggestions));
    } catch { return partial(); }
  };
}

export function normalizeCommands(raw, skillNames) {
  const commands = [];
  const seen = new Set();
  let partial = false;
  let truncated = false;
  let retainedBytes = 0;
  const bounded = (value, limit) => {
    if (typeof value === 'string' && value.length > limit) truncated = true;
    return text(value, limit);
  };
  if (!Array.isArray(raw)) throw new Error('Invalid command metadata');
  if (raw.length > 16384) truncated = true;
  const priority = new Map([['extension', 0], ['skill', 1], ['prompt', 2]]);
  const entries = raw.slice(0, 16384).sort((a, b) => (priority.get(a?.source) ?? 3) - (priority.get(b?.source) ?? 3));
  for (const entry of entries) {
    if (!entry || !['extension', 'prompt', 'skill'].includes(entry.source)
      || !/^[A-Za-z0-9][A-Za-z0-9._:-]{0,119}$/.test(entry.name)) {
      partial = true;
      continue;
    }
    if (seen.has(entry.name)) continue;
    seen.add(entry.name);
    if (entry.source === 'skill' && !skillNames.has(entry.name)) continue;
    if (commands.length === maxEntries) { truncated = true; continue; }
    const info = entry.sourceInfo;
    if (!info || typeof info.path !== 'string' || !info.path || typeof info.source !== 'string' || !info.source
      || !['user', 'project', 'temporary'].includes(info.scope)
      || !['package', 'top-level'].includes(info.origin)) { partial = true; continue; }
    const command = {
      command: `/${entry.name}`, description: bounded(entry.description || entry.name, 240),
      source: info.scope === 'user' ? 'personal' : info.scope,
      kind: entry.source,
      provenance: {
        path: bounded(sanitizePath(info.path), 1024), source: bounded(sanitizeSource(info.source), 256), scope: info.scope, origin: info.origin,
        ...(info.baseDir ? { base_dir: bounded(sanitizePath(info.baseDir), 1024) } : {}),
      },
    };
    retainedBytes += Buffer.byteLength(JSON.stringify(command)) + 1;
    if (retainedBytes > maxBytes - 16384) { truncated = true; break; }
    commands.push(command);
  }
  commands.sort((a, b) => a.command < b.command ? -1 : a.command > b.command ? 1 : 0);
  return { commands, status: partial ? 'partial' : 'available', truncated };
}

export async function instanceIdentity(socketPath) {
  const path = await realpath(socketPath);
  const stat = await lstat(path, { bigint: true });
  if (!stat.isSocket() || stat.uid !== BigInt(process.getuid())) throw new Error('Invalid Herdr socket');
  return createHash('sha256').update(`${path}\n${stat.dev}\n${stat.ino}`).digest('hex');
}

export async function startBridge({ instance, pane, session, pid = process.pid, directory, catalog }) {
  await mkdir(directory, { mode: 0o700 }).catch(error => { if (error.code !== 'EEXIST') throw error; });
  return listen();
  async function listen() {
    const stat = await lstat(directory);
    if (!stat.isDirectory() || stat.isSymbolicLink() || stat.uid !== process.getuid() || (stat.mode & 0o777) !== 0o700) {
      throw new Error('Unsafe runtime directory');
    }
    const path = join(directory, `${pid}.sock`);
    const incarnation = randomUUID();
    const sockets = new Set();
    let closed = false;
    let busy = false;
    const server = createServer(socket => {
      if (closed || sockets.size >= 4) { socket.destroy(); return; }
      sockets.add(socket);
      const controller = new AbortController();
      const deadline = setTimeout(() => {
        controller.abort(new Error('Discovery timed out'));
        socket.destroy();
      }, 800);
      socket.on('close', () => {
        clearTimeout(deadline);
        controller.abort(new Error('Discovery disconnected'));
        sockets.delete(socket);
      });
      socket.on('error', () => {});
      socket.setTimeout(1000, () => socket.destroy());
      let input = '';
      socket.on('data', chunk => {
        input += chunk.toString('utf8');
        if (input.length > 4096) { socket.destroy(); return; }
        if (!input.includes('\n')) return;
        socket.removeAllListeners('data');
        void respond().catch(() => socket.destroy());
      });
      async function respond() {
        const request = JSON.parse(input);
        if (!request || typeof request !== 'object' || Array.isArray(request)
          || busy || request.instance !== instance || request.pane !== pane || request.session !== session
          || request.pid !== pid || !/^[a-f0-9]{32}$/.test(request.challenge)) { socket.destroy(); return; }
        busy = true;
        try {
          const result = await abortable(Promise.resolve().then(() => catalog(controller.signal)), controller.signal);
          if (closed || socket.destroyed) return;
          const response = { ...result, instance, pane, session, pid, incarnation, challenge: request.challenge };
          response.revision = createHash('sha256').update(JSON.stringify([response.status, response.truncated, response.commands])).digest('hex');
          const payload = JSON.stringify(response);
          if (Buffer.byteLength(payload) >= maxBytes) { socket.destroy(); return; }
          socket.end(`${payload}\n`);
        } catch { socket.destroy(); }
        finally { busy = false; }
      }
    });
    await new Promise((resolve, reject) => {
      server.once('error', reject);
      server.listen(path, () => { server.removeListener('error', reject); resolve(); });
    });
    server.on('error', () => {});
    try { await chmod(path, 0o600); }
    catch (error) { await new Promise(resolve => server.close(resolve)); throw error; }
    server.unref();
    return async () => {
      if (closed) return;
      closed = true;
      for (const socket of sockets) socket.destroy();
      await new Promise(resolve => server.close(resolve));
    };
  }
}

export default function commandBridge(pi) {
  let close;
  let owner;
  let provider;
  let discovering = false;
  let ready = false;
  pi.on('resources_discover', () => { discovering = true; });
  pi.on('session_start', async (_event, ctx) => {
    if (ctx.mode !== 'tui' || !process.stdin.isTTY || globalThis[ownerKey]) return;
    const pane = process.env.HERDR_PANE_ID;
    const socketPath = process.env.HERDR_SOCKET_PATH;
    const session = ctx.sessionManager.getSessionId();
    if (!pane || pane.length > 160 || !socketPath || !session) return;
    owner = {};
    globalThis[ownerKey] = owner;
    try {
      const instance = await instanceIdentity(socketPath);
      ctx.ui.addAutocompleteProvider(current => {
        provider = current;
        if (discovering) ready = true;
        return current;
      });
      const directory = `/tmp/herdr-pi-${process.getuid()}-${instance.slice(0, 16)}`;
      close = await startBridge({ instance, pane, session, directory,
        catalog: createCatalog(() => pi.getCommands(), () => ready ? provider : undefined),
      });
    } catch {
      if (globalThis[ownerKey] === owner) delete globalThis[ownerKey];
    }
  });
  pi.on('session_shutdown', async () => {
    ready = false;
    await close?.();
    close = undefined;
    if (owner && globalThis[ownerKey] === owner) delete globalThis[ownerKey];
  });
}
