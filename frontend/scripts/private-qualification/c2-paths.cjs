'use strict';

const fs = module.require('node:fs');
const path = module.require('node:path');
const crypto = module.require('node:crypto');
const { constants } = fs;

function identity(stat) {
  return {
    dev: String(stat.dev),
    ino: String(stat.ino),
    uid: Number(stat.uid),
    mode: Number(stat.mode),
    nlink: Number(stat.nlink),
    size: Number(stat.size),
    file: stat.isFile(),
    directory: stat.isDirectory(),
    symlink: stat.isSymbolicLink()
  };
}

function sameObject(left, right) {
  return left.dev === right.dev && left.ino === right.ino && left.uid === right.uid && left.mode === right.mode && (!left.file || left.nlink === right.nlink) && left.file === right.file && left.directory === right.directory && left.symlink === right.symlink;
}

function safeParts(relative) {
  if (typeof relative !== 'string' || !relative || path.isAbsolute(relative))
    throw new Error('Private path must be a non-empty relative path');
  const parts = relative.split(/[\\/]+/);
  if (parts.some(part => !part || part === '.' || part === '..' || part.includes('\0')))
    throw new Error('Private path contains an unsafe component');
  return parts;
}

function checkedStat(target, expectedUid, kind, expected) {
  const details = identity(fs.lstatSync(target));
  if (details.symlink || details.uid !== expectedUid || details.file && details.nlink !== 1)
    throw new Error(`Private path identity refused: ${target}`);
  if (kind === 'directory' && !details.directory || kind === 'file' && !details.file)
    throw new Error(`Private path type refused: ${target}`);
  if (expected && !sameObject(expected, details))
    throw new Error(`Private path changed identity: ${target}`);
  if (fs.realpathSync(target) !== target)
    throw new Error(`Private path resolution escaped its pin: ${target}`);
  return details;
}

function pinExistingAncestry(parentPath) {
  const absolute = path.resolve(parentPath);
  const root = path.parse(absolute).root;
  const parts = absolute.slice(root.length).split(path.sep).filter(Boolean);
  const pins = [];
  let cursor = root;
  pins.push({ path: cursor, identity: identity(fs.lstatSync(cursor)) });
  for (const part of parts) {
    cursor = path.join(cursor, part);
    const stat = fs.lstatSync(cursor);
    if (stat.isSymbolicLink() || !stat.isDirectory() || fs.realpathSync(cursor) !== cursor)
      throw new Error(`Private parent ancestry refused: ${cursor}`);
    pins.push({ path: cursor, identity: identity(stat) });
  }
  return pins;
}

function verifyAncestry(pins) {
  for (const pin of pins) {
    const current = identity(fs.lstatSync(pin.path));
    if (!sameObject(pin.identity, current) || current.symlink || !current.directory && pin.path !== path.parse(pin.path).root)
      throw new Error(`Private parent ancestry changed: ${pin.path}`);
    if (fs.realpathSync(pin.path) !== pin.path)
      throw new Error(`Private parent ancestry resolved through a link: ${pin.path}`);
  }
}

function createStageRoot(stageRoot) {
  const absolute = path.resolve(stageRoot);
  if (!absolute.startsWith('/private/tmp/'))
    throw new Error('C2 stage root must be a new direct child of /private/tmp');
  const parent = path.dirname(absolute);
  const ancestry = pinExistingAncestry(parent);
  verifyAncestry(ancestry);
  fs.mkdirSync(absolute, { mode: 0o700 });
  const expectedUid = process.getuid?.();
  if (!Number.isInteger(expectedUid))
    throw new Error('Current user identity is unavailable');
  const rootIdentity = checkedStat(absolute, expectedUid, 'directory');
  if ((rootIdentity.mode & 0o077) !== 0)
    throw new Error('C2 stage root permissions are not private');
  return { path: absolute, parent, parentAncestry: ancestry, identity: rootIdentity, uid: expectedUid };
}

function openExclusiveFile(target, content, expectedUid) {
  const bytes = Buffer.isBuffer(content) ? content : Buffer.from(String(content));
  let descriptor;
  try {
    descriptor = fs.openSync(target, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | (constants.O_NOFOLLOW || 0), 0o600);
    const before = identity(fs.fstatSync(descriptor));
    if (!before.file || before.symlink || before.uid !== expectedUid || before.nlink !== 1)
      throw new Error(`Private file descriptor identity refused: ${target}`);
    let offset = 0;
    while (offset < bytes.length) {
      const written = fs.writeSync(descriptor, bytes, offset, bytes.length - offset);
      if (!written) throw new Error(`Private file write made no progress: ${target}`);
      offset += written;
    }
    fs.fsyncSync(descriptor);
    const after = identity(fs.fstatSync(descriptor));
    if (!sameObject(before, after))
      throw new Error(`Private file descriptor changed while writing: ${target}`);
    const named = checkedStat(target, expectedUid, 'file', after);
    return named;
  } finally {
    if (descriptor !== undefined) fs.closeSync(descriptor);
  }
}

class C2Paths {
  constructor(stage, invocationPath, invocationId, existing = {}) {
    if (!stage || !path.isAbsolute(invocationPath) || !/^[0-9a-f-]{36}$/.test(invocationId))
      throw new Error('C2 invocation identity is invalid');
    this.stage = stage;
    this.root = path.resolve(invocationPath);
    this.id = invocationId;
    this.uid = stage.uid;
    this.sealed = false;
    this.objects = new Map();
    this.actors = new Map();
    this.failure = null;
    verifyAncestry(stage.parentAncestry);
    checkedStat(stage.path, stage.uid, 'directory', stage.identity);
    if (path.dirname(this.root) !== stage.path || !this.root.startsWith(`${stage.path}${path.sep}`))
      throw new Error('C2 invocation must be a direct child of the stage root');
    if (existing.entries) {
      this.rootIdentity = checkedStat(this.root, this.uid, 'directory', existing.rootIdentity);
    } else {
      fs.mkdirSync(this.root, { mode: 0o700 });
      this.rootIdentity = checkedStat(this.root, this.uid, 'directory');
    }
    if ((this.rootIdentity.mode & 0o077) !== 0)
      throw new Error('C2 invocation permissions are not private');
    this.objects.set(this.root, { identity: this.rootIdentity, kind: 'directory', shared: true, actor: 'controller' });
    this.registerActor('controller', this.root);
    if (existing.entries) {
      const entries = [...existing.entries, ...(existing.additionalEntries || [])];
      for (const entry of entries) {
        if (entry.path === '.') continue;
        const parts = safeParts(entry.path);
        const target = path.resolve(this.root, ...parts);
        if (!target.startsWith(`${this.root}${path.sep}`) || this.objects.has(target))
          throw new Error(`Attached C2 route path is duplicated or outside its invocation: ${entry.path}`);
        const kind = entry.kind;
        const expected = { dev: String(entry.dev), ino: String(entry.ino), uid: Number(entry.uid), mode: Number(entry.mode), nlink: Number(entry.nlink), size: Number(entry.bytes), file: kind === 'file', directory: kind === 'directory', symlink: false };
        const observed = checkedStat(target, this.uid, kind, expected);
        if (kind === 'file' && entry.hash && crypto.createHash('sha256').update(fs.readFileSync(target)).digest('hex') !== entry.hash)
          throw new Error(`Attached C2 source content changed: ${entry.path}`);
        this.objects.set(target, { identity: observed, kind, shared: kind === 'directory', actor: `attached:${invocationId}` });
      }
    }
  }
  fail(error) {
    this.sealed = true;
    this.failure = this.failure || error;
    return this.failure;
  }
  assertOpen() {
    if (this.sealed) throw new Error(`C2 path admission sealed: ${this.failure?.message || 'prior refusal'}`);
    verifyAncestry(this.stage.parentAncestry);
    checkedStat(this.stage.path, this.uid, 'directory', this.stage.identity);
    checkedStat(this.root, this.uid, 'directory', this.rootIdentity);
  }
  registerActor(actor, base = this.root) {
    try {
      this.assertOpen();
      if (typeof actor !== 'string' || !actor || actor.includes('\0'))
        throw new Error('C2 writer actor identity is invalid');
      const target = path.resolve(base);
      const lease = this.objects.get(target);
      if (!lease || lease.kind !== 'directory')
        throw new Error(`C2 writer actor has no enrolled directory: ${actor}`);
      const prior = this.actors.get(actor);
      if (prior && prior !== target)
        throw new Error(`C2 writer actor identity was reused: ${actor}`);
      this.actors.set(actor, target);
      return { actor, path: target, identity: lease.identity };
    } catch (error) {
      throw this.fail(error);
    }
  }
  actorBase(actor) {
    this.assertOpen();
    const base = this.actors.get(actor);
    if (!base) throw new Error(`C2 writer actor was not enrolled: ${actor}`);
    const lease = this.objects.get(base);
    checkedStat(base, this.uid, 'directory', lease.identity);
    return base;
  }
  acquireDirectory(relative, actor, options = {}) {
    try {
      const base = this.actorBase(actor);
      const parts = safeParts(relative);
      let cursor = base;
      for (const part of parts) {
        const next = path.join(cursor, part);
        const existing = this.objects.get(next);
        if (existing) {
          if (existing.kind !== 'directory') throw new Error(`Private path is not a directory: ${next}`);
          checkedStat(next, this.uid, 'directory', existing.identity);
          if (existing.actor !== actor && !this.objects.get(cursor)?.shared)
            throw new Error(`Private directory belongs to another writer: ${next}`);
          cursor = next;
          continue;
        }
        if (fs.existsSync(next)) throw new Error(`Private path exists without an enrollment: ${next}`);
        const parentLease = this.objects.get(cursor);
        if (!parentLease || parentLease.kind !== 'directory' || parentLease.actor !== actor && !parentLease.shared)
          throw new Error(`Private parent was not admitted to this writer: ${cursor}`);
        fs.mkdirSync(next, { mode: options.mode || 0o700 });
        const stat = checkedStat(next, this.uid, 'directory');
        if ((stat.mode & 0o077) !== 0) throw new Error(`Private directory permissions are not private: ${next}`);
        const lease = { identity: stat, kind: 'directory', shared: !!options.shareChildren, actor };
        this.objects.set(next, lease);
        cursor = next;
      }
      this.actors.set(actor, cursor);
      return { actor, path: cursor, identity: this.objects.get(cursor).identity };
    } catch (error) {
      throw this.fail(error);
    }
  }
  acquireSharedDirectory(relative, actor) {
    const lease = this.acquireDirectory(relative, actor, { shareChildren: true });
    const object = this.objects.get(lease.path);
    object.shared = true;
    return lease;
  }
  acquireFile(relative, actor, content = '') {
    try {
      const base = this.actorBase(actor);
      const parts = safeParts(relative);
      const filename = parts.pop();
      let parent = base;
      if (parts.length) parent = this.acquireSharedDirectory(parts.join('/'), actor).path;
      const target = path.join(parent, filename);
      if (this.objects.has(target) || fs.existsSync(target))
        throw new Error(`Private file collision refused: ${target}`);
      const parentLease = this.objects.get(parent);
      if (!parentLease || parentLease.kind !== 'directory' || parentLease.actor !== actor && !parentLease.shared)
        throw new Error(`Private file parent was not admitted to this writer: ${parent}`);
      const stat = openExclusiveFile(target, content, this.uid);
      this.objects.set(target, { identity: stat, kind: 'file', actor });
      return { actor, path: target, identity: stat };
    } catch (error) {
      throw this.fail(error);
    }
  }
  continueFile(target, actor, write) {
    try {
      if (this.sealed) {
        verifyAncestry(this.stage.parentAncestry);
        checkedStat(this.stage.path, this.uid, 'directory', this.stage.identity);
        checkedStat(this.root, this.uid, 'directory', this.rootIdentity);
      } else {
        this.assertOpen();
      }
      const absolute = path.resolve(target);
      const lease = this.objects.get(absolute);
      if (!lease || lease.kind !== 'file' || lease.actor !== actor)
        throw new Error(`Private file continuation was not admitted: ${absolute}`);
      const before = checkedStat(absolute, this.uid, 'file', lease.identity);
      const descriptor = fs.openSync(absolute, constants.O_WRONLY | constants.O_APPEND | (constants.O_NOFOLLOW || 0));
      try {
        const opened = identity(fs.fstatSync(descriptor));
        if (!sameObject(before, opened)) throw new Error(`Private file descriptor no longer matches its pin: ${absolute}`);
        const bytes = Buffer.isBuffer(write) ? write : Buffer.from(String(write));
        let offset = 0;
        while (offset < bytes.length) {
          const written = fs.writeSync(descriptor, bytes, offset, bytes.length - offset);
          if (!written) throw new Error(`Private file append made no progress: ${absolute}`);
          offset += written;
        }
        fs.fsyncSync(descriptor);
        const after = identity(fs.fstatSync(descriptor));
        if (!sameObject(before, after)) throw new Error(`Private file changed while appending: ${absolute}`);
        lease.identity = after;
      } finally {
        fs.closeSync(descriptor);
      }
      checkedStat(absolute, this.uid, 'file', lease.identity);
      return absolute;
    } catch (error) {
      throw this.fail(error);
    }
  }
  rewriteFile(target, actor, write) {
    try {
      if (this.sealed) {
        verifyAncestry(this.stage.parentAncestry);
        checkedStat(this.stage.path, this.uid, 'directory', this.stage.identity);
        checkedStat(this.root, this.uid, 'directory', this.rootIdentity);
      } else {
        this.assertOpen();
      }
      const absolute = path.resolve(target);
      const lease = this.objects.get(absolute);
      if (!lease || lease.kind !== 'file' || lease.actor !== actor)
        throw new Error(`Private file rewrite was not admitted: ${absolute}`);
      const before = checkedStat(absolute, this.uid, 'file', lease.identity);
      const descriptor = fs.openSync(absolute, constants.O_WRONLY | (constants.O_NOFOLLOW || 0));
      try {
        const opened = identity(fs.fstatSync(descriptor));
        if (!sameObject(before, opened)) throw new Error(`Private file descriptor no longer matches its pin: ${absolute}`);
        fs.ftruncateSync(descriptor, 0);
        const bytes = Buffer.isBuffer(write) ? write : Buffer.from(String(write));
        let offset = 0;
        while (offset < bytes.length) {
          const written = fs.writeSync(descriptor, bytes, offset, bytes.length - offset);
          if (!written) throw new Error(`Private file rewrite made no progress: ${absolute}`);
          offset += written;
        }
        fs.fsyncSync(descriptor);
        const after = identity(fs.fstatSync(descriptor));
        if (!sameObject(before, after)) throw new Error(`Private file changed while rewriting: ${absolute}`);
        lease.identity = after;
      } finally {
        fs.closeSync(descriptor);
      }
      checkedStat(absolute, this.uid, 'file', lease.identity);
      return absolute;
    } catch (error) {
      throw this.fail(error);
    }
  }
  admitProducedFile(target, actor) {
    try {
      this.assertOpen();
      const absolute = path.resolve(target);
      const leaseBase = this.actors.get(actor);
      if (!leaseBase || path.dirname(absolute) !== leaseBase)
        throw new Error(`Produced file is outside its enrolled writer directory: ${absolute}`);
      const parent = this.objects.get(leaseBase);
      if (!parent || parent.kind !== 'directory')
        throw new Error(`Produced file parent is not enrolled: ${leaseBase}`);
      checkedStat(leaseBase, this.uid, 'directory', parent.identity);
      if (!fs.existsSync(absolute) || this.objects.has(absolute))
        throw new Error(`Produced file is missing or already enrolled: ${absolute}`);
      const stat = checkedStat(absolute, this.uid, 'file');
      this.objects.set(absolute, { identity: stat, kind: 'file', actor, externallyProduced: true });
      return { actor, path: absolute, identity: stat };
    } catch (error) {
      throw this.fail(error);
    }
  }
  retainPath(target, actor) {
    try {
      this.assertOpen();
      const absolute = path.resolve(target);
      const lease = this.objects.get(absolute);
      if (!lease || lease.actor !== actor)
        throw new Error(`Private retention target was not admitted: ${absolute}`);
      const retained = checkedStat(absolute, this.uid, lease.kind, lease.identity);
      lease.identity = retained;
      return retained;
    } catch (error) {
      throw this.fail(error);
    }
  }
  relocate(source, destination, actor) {
    try {
      this.assertOpen();
      const from = path.resolve(source);
      const to = path.resolve(destination);
      const lease = this.objects.get(from);
      if (!lease || lease.actor !== actor || lease.kind !== 'file')
        throw new Error(`Private relocation source was not admitted: ${from}`);
      checkedStat(from, this.uid, 'file', lease.identity);
      const parent = this.objects.get(path.dirname(to));
      if (!parent || parent.kind !== 'directory' || parent.actor !== actor && !parent.shared)
        throw new Error(`Private relocation destination parent was not admitted: ${to}`);
      if (this.objects.has(to) || fs.existsSync(to))
        throw new Error(`Private relocation destination collision: ${to}`);
      fs.renameSync(from, to);
      const moved = checkedStat(to, this.uid, 'file', lease.identity);
      this.objects.delete(from);
      this.objects.set(to, { identity: moved, kind: 'file', actor, relocatedFrom: from });
      return { source: from, destination: to, identity: moved };
    } catch (error) {
      throw this.fail(error);
    }
  }
  snapshot() {
    return {
      invocationId: this.id,
      root: this.root,
      sealed: this.sealed,
      failure: this.failure ? String(this.failure.message || this.failure) : null,
      entries: [...this.objects].map(([target, lease]) => ({
        path: path.relative(this.root, target) || '.',
        kind: lease.kind,
        actor: lease.actor,
        dev: lease.identity.dev,
        ino: lease.identity.ino,
        uid: lease.identity.uid,
        mode: lease.identity.mode,
        nlink: lease.identity.nlink,
        bytes: lease.identity.size,
        hash: lease.kind === 'file' ? crypto.createHash('sha256').update(fs.readFileSync(target)).digest('hex') : undefined
      }))
    };
  }
}

function writeStageFile(stage, name, content) {
  if (!stage || !['stage-root.json', 'journal.ndjson', 'supplemental-allowance-20261006.json', 'supplemental-allowance-amendment-20261006.json', 'supplemental-allowance-amendment-02-20261006.json', 'attempt8-static-preflight.json', 'attempt8-static-preflight-revision-02.json', 'attempt8-static-preflight-revision-03.json', 'attempt10-static-preflight.json'].includes(name)) throw new Error('C2 stage sidecar name is not authorized');
  verifyAncestry(stage.parentAncestry);
  checkedStat(stage.path, stage.uid, 'directory', stage.identity);
  const target = path.join(stage.path, name);
  return { path: target, identity: openExclusiveFile(target, content, stage.uid) };
}

function readStageRoot(stageRoot) {
  const absolute = path.resolve(stageRoot);
  if (!absolute.startsWith('/private/tmp/') || path.dirname(absolute) !== '/private/tmp')
    throw new Error('C2 stage root must be a direct child of /private/tmp');
  const parentAncestry = pinExistingAncestry(path.dirname(absolute));
  verifyAncestry(parentAncestry);
  const uid = process.getuid?.();
  if (!Number.isInteger(uid)) throw new Error('Current user identity is unavailable');
  const rootIdentity = checkedStat(absolute, uid, 'directory');
  if ((rootIdentity.mode & 0o077) !== 0) throw new Error('C2 stage root permissions are not private');
  const recordPath = path.join(absolute, 'stage-root.json');
  const before = checkedStat(recordPath, uid, 'file');
  if ((before.mode & 0o077) !== 0) throw new Error('C2 stage root record permissions are not private');
  const bytes = fs.readFileSync(recordPath);
  const after = checkedStat(recordPath, uid, 'file', before);
  if (bytes.length !== after.size) throw new Error('C2 stage root record changed while reading');
  const record = JSON.parse(bytes.toString('utf8'));
  const ancestryMatches = Array.isArray(record.parentAncestry) && record.parentAncestry.length === parentAncestry.length && record.parentAncestry.every((pin, index) => pin.path === parentAncestry[index].path && sameObject(pin.identity, parentAncestry[index].identity));
  if (record.schema !== 1 || record.path !== absolute || !sameObject(record.identity, rootIdentity) || !ancestryMatches)
    throw new Error('Existing C2 stage root has no matching exclusive creation record');
  let journal;
  const journalPath = path.join(absolute, 'journal.ndjson');
  try {
    const journalIdentity = checkedStat(journalPath, uid, 'file');
    journal = { path: journalPath, identity: journalIdentity };
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
  }
  return { path: absolute, parent: path.dirname(absolute), parentAncestry, identity: rootIdentity, uid, journal };
}

function appendStageFile(stage, target, expected, content) {
  verifyAncestry(stage.parentAncestry);
  checkedStat(stage.path, stage.uid, 'directory', stage.identity);
  const absolute = path.resolve(target);
  if (path.dirname(absolute) !== stage.path) throw new Error('C2 stage append escaped its root');
  const before = checkedStat(absolute, stage.uid, 'file', expected);
  const descriptor = fs.openSync(absolute, constants.O_WRONLY | constants.O_APPEND | (constants.O_NOFOLLOW || 0));
  try {
    if (!sameObject(before, identity(fs.fstatSync(descriptor)))) throw new Error('C2 stage append descriptor differs from its pin');
    const bytes = Buffer.isBuffer(content) ? content : Buffer.from(String(content));
    let offset = 0;
    while (offset < bytes.length) {
      const written = fs.writeSync(descriptor, bytes, offset, bytes.length - offset);
      if (!written) throw new Error('C2 stage append made no progress');
      offset += written;
    }
    fs.fsyncSync(descriptor);
    const after = identity(fs.fstatSync(descriptor));
    if (!sameObject(before, after)) throw new Error('C2 stage append changed the pinned object');
    checkedStat(absolute, stage.uid, 'file', after);
    return after;
  } finally {
    fs.closeSync(descriptor);
  }
}

function createInvocation(stage, invocationPath, invocationId) {
  return new C2Paths(stage, invocationPath, invocationId);
}

function attachInvocation(stage, invocationPath, invocationId, entries, additionalEntries = []) {
  if (!Array.isArray(entries) || !Array.isArray(additionalEntries)) throw new Error('Attached C2 route manifest is malformed');
  const rootEntry = entries.find(entry => entry.path === '.' && entry.kind === 'directory');
  if (!rootEntry) throw new Error('Attached C2 route manifest omitted its invocation root pin');
  const rootIdentity = { dev: String(rootEntry.dev), ino: String(rootEntry.ino), uid: Number(rootEntry.uid), mode: Number(rootEntry.mode), nlink: Number(rootEntry.nlink), size: Number(rootEntry.bytes), file: false, directory: true, symlink: false };
  return new C2Paths(stage, invocationPath, invocationId, { entries, additionalEntries, rootIdentity });
}

function reconcileRetentionFailure(result, exceeded, retention, message) {
  if (!exceeded) return false;
  if (result?.phase !== 'worker' || !result.verification || !retention || typeof message !== 'string')
    throw new Error('C2 retention failure receipt inputs are incomplete');
  result.status = 'failed';
  result.error = [result.error, message].filter(Boolean).join('\n');
  result.verification.retention = { status: 'failed', ...retention, receiptsReconciled: true };
  return true;
}

function attemptSummary(result) {
  return { schema: 1, phase: result.phase, attempt: result.attempt, status: result.status, invocationId: result.invocationId, cases: result.cases, passed: result.passed, failed: result.failed, variants: result.variants, nativeEffects: result.nativeEffects, ...(result.verification?.retention ? { retention: result.verification.retention } : {}) };
}

function terminalRecord(result, completedAt) {
  return { type: 'TERMINAL', attempt: result.attempt, phase: result.phase, invocationId: result.invocationId, status: result.status, completedAt };
}

module.exports = { C2Paths, createStageRoot, readStageRoot, writeStageFile, appendStageFile, createInvocation, attachInvocation, identity, sameObject, safeParts, pinExistingAncestry, verifyAncestry, checkedStat, reconcileRetentionFailure, attemptSummary, terminalRecord };
