'use strict';

const {createGunzip} = require('node:zlib');
const {createHash} = require('node:crypto');
const {TextDecoder} = require('node:util');
const COMPRESSED_LIMIT = 262144;
const CONFIG_LIMIT = 2097152;
const BOOT = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/;
const source = '/proc/config.gz';

async function acquireKernelCapability(reader, deadline, check) {
  const acquiredStartedAt = Date.now();
  const guard = () => {
    check();
    if (!Number.isSafeInteger(deadline) || Date.now() >= deadline) throw new Error('Kernel capability deadline');
  };
  const boot = async () => {
    guard();
    const value = await reader.read(['shell', 'cat', '/proc/sys/kernel/random/boot_id'], deadline);
    guard();
    if (!BOOT.test(value)) throw new Error('Kernel capability boot unavailable');
    return value;
  };
  const bootId = await boot();
  const compressed = await reader.readKernelConfig(deadline);
  guard();
  if (!Buffer.isBuffer(compressed) || !compressed.length || compressed.length > COMPRESSED_LIMIT) throw new Error('Kernel compressed configuration bound');
  const bytes = await new Promise((resolve, reject) => {
    const stream = createGunzip();
    const chunks = [];
    let length = 0;
    let settled = false;
    const finish = (error) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      stream.destroy();
      if (error) reject(error); else resolve(Buffer.concat(chunks, length));
    };
    const timer = setTimeout(() => finish(new Error('Kernel decompression deadline')), Math.max(1, deadline - Date.now()));
    stream.on('error', () => finish(new Error('Kernel configuration gzip unavailable')));
    stream.on('data', chunk => {
      try {
        guard();
        length += chunk.length;
        if (length > CONFIG_LIMIT) throw new Error('Kernel decompressed configuration bound');
        chunks.push(chunk);
      } catch (error) { finish(error); }
    });
    stream.on('end', () => { try { guard(); finish(); } catch (error) { finish(error); } });
    stream.end(compressed);
  });
  guard();
  const text = new TextDecoder('utf-8', {fatal: true}).decode(bytes);
  if (text.includes('\0') || !text.endsWith('\n')) throw new Error('Kernel configuration malformed');
  for (const name of ['CONFIG_IKCONFIG', 'CONFIG_IKCONFIG_PROC']) {
    const fields = text.split('\n').filter(line => new RegExp(`\\b${name}\\b`).test(line));
    if (fields.length !== 1 || fields[0] !== `${name}=y`) throw new Error(`Built-in kernel configuration unavailable: field=${name} settings=${fields.length}`);
  }
  const settings = text.split('\n').filter(line => line.includes('CONFIG_PID_NS'));
  if (settings.length !== 1 || !['CONFIG_PID_NS=y', '# CONFIG_PID_NS is not set'].includes(settings[0])) {
    throw new Error(`Kernel PID namespace configuration unavailable: settings=${settings.length}`);
  }
  if (await boot() !== bootId) throw new Error('Kernel capability boot changed during acquisition');
  guard();
  return Object.freeze({mode: settings[0] === 'CONFIG_PID_NS=y' ? 'enabled' : 'disabled', source,
    sha256: createHash('sha256').update(bytes).digest('hex'), compressedBytes: compressed.length, configBytes: bytes.length,
    compressedLimit: COMPRESSED_LIMIT, configLimit: CONFIG_LIMIT, bootId, acquiredStartedAt, acquiredFinishedAt: Date.now()});
}

function namespaceForCapability(capability) {
  if (capability.mode === 'enabled') return 'reader-and-browser-active-in-procfs-mount-pid-namespace';
  if (capability.mode === 'disabled') return 'kernel-pid-namespaces-disabled';
  throw new Error('Unknown kernel PID namespace capability');
}

function validateNamespaceStatus(text, expectedPid, capability) {
  if (typeof text !== 'string' || text.length > 65536) throw new Error('Native namespace metadata bound');
  const lines = text.split(/\r?\n/);
  const pids = lines.filter(line => /^Pid\b/.test(line));
  const namespaces = lines.filter(line => /^NS(?:pid|tgid|pgid|sid)\b/.test(line));
  const nspids = lines.filter(line => /^NSpid\b/.test(line));
  const visible = pids[0]?.slice(4).trim();
  const refuse = () => { throw new Error(`Native namespace metadata refused: mode=${capability.mode} Pid=${pids.length} NSpid=${nspids.length} namespaceFields=${namespaces.length}`); };
  if (pids.length !== 1 || !pids[0].startsWith('Pid:') || !/^[1-9]\d*$/.test(visible || '') || Number(visible) > 2147483647 || (expectedPid && visible !== expectedPid)) refuse();
  if (capability.mode === 'disabled') { if (namespaces.length) refuse(); return; }
  if (capability.mode !== 'enabled' || nspids.length !== 1 || !nspids[0].startsWith('NSpid:') || nspids[0].slice(6).trim() !== visible) refuse();
}

function validKernelCapability(c, bootId, end) {
  return c && typeof c === 'object' && !Array.isArray(c)
    && Object.keys(c).sort().join(',') === 'acquiredFinishedAt,acquiredStartedAt,bootId,compressedBytes,compressedLimit,configBytes,configLimit,mode,sha256,source'
    && ['enabled', 'disabled'].includes(c.mode) && c.source === source && /^[0-9a-f]{64}$/.test(c.sha256) && c.bootId === bootId && BOOT.test(c.bootId)
    && c.compressedLimit === COMPRESSED_LIMIT && c.configLimit === CONFIG_LIMIT
    && Number.isSafeInteger(c.compressedBytes) && c.compressedBytes > 0 && c.compressedBytes <= COMPRESSED_LIMIT
    && Number.isSafeInteger(c.configBytes) && c.configBytes > 0 && c.configBytes <= CONFIG_LIMIT
    && Number.isSafeInteger(c.acquiredStartedAt) && c.acquiredStartedAt > 0 && Number.isSafeInteger(c.acquiredFinishedAt)
    && c.acquiredStartedAt <= c.acquiredFinishedAt && c.acquiredFinishedAt <= end;
}

function sameKernelCapability(a, b) {
  return ['mode', 'source', 'sha256', 'compressedBytes', 'configBytes', 'compressedLimit', 'configLimit', 'bootId'].every(key => a?.[key] === b?.[key]);
}

module.exports = {acquireKernelCapability, namespaceForCapability, validateNamespaceStatus, validKernelCapability, sameKernelCapability};
