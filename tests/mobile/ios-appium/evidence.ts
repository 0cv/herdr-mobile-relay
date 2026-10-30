import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { open, readFile, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const maximumBytes = 131072;
const digest = (value: unknown): value is string => typeof value === 'string' && /^[a-f0-9]{64}$/u.test(value);
const pathValue = (value: unknown): value is string => typeof value === 'string' && value.length <= 4096 && /^\/[A-Za-z0-9_./@+-]+$/u.test(value) && resolve(value) === value;
function hashMap(value: Record<string, unknown>, keys: string[]): void {
  assert.ok(value && typeof value === 'object' && !Array.isArray(value));
  assert.deepEqual(Object.keys(value).sort(), keys.sort());
  assert.ok(Object.values(value).every(digest));
}

function exactKeys(value: Record<string, unknown>, keys: string[]): void {
  assert.ok(value && typeof value === 'object' && !Array.isArray(value));
  assert.deepEqual(Object.keys(value).sort(), keys.sort());
}

async function copyIntegrity(home: string, output: string): Promise<void> {
  const handle = await open(join(home, 'installed-context-integrity.json'), 'r');
  let bytes: Buffer;
  try {
    assert.ok((await handle.stat()).size <= maximumBytes);
    const buffer = Buffer.alloc(maximumBytes + 1);
    const { bytesRead } = await handle.read(buffer, 0, buffer.length, 0);
    assert.ok(bytesRead > 0 && bytesRead <= maximumBytes);
    bytes = buffer.subarray(0, bytesRead);
  } finally { await handle.close(); }
  const record = JSON.parse(bytes.toString('utf8'));
  exactKeys(record, ['home', 'node', 'nodeVersion', 'nodeSha256', 'main', 'entries', 'contract', 'attestation', 'selected']);
  const manifestBytes = await readFile(new URL('producer-manifest.json', import.meta.url));
  const manifest = JSON.parse(manifestBytes.toString('utf8'));
  assert.equal(record.contract, 'herdr-installed-contexts-v1');
  assert.equal(record.nodeVersion, '26.13.0');
  assert.ok(digest(record.nodeSha256));
  for (const path of [record.home, record.node, record.main]) assert.ok(pathValue(path));
  assert.equal(record.home, resolve(home));
  const entries = Object.fromEntries(Object.entries(manifest.packages).map(([id, pkg]: [string, any]) => [id, new URL(`file://${join(home, pkg.root, pkg.entry)}`).href]));
  assert.deepEqual(record.entries, entries);
  assert.equal(record.main, fileURLToPath(entries.appium));
  exactKeys(record.selected, ['name', 'version', 'installPath', 'mainClass']);
  assert.deepEqual(record.selected, { name: 'appium-xcuitest-driver', version: '12.12.4',
    installPath: join(home, manifest.packages.xcuitest.root), mainClass: 'XCUITestDriver' });
  const captured = record.attestation;
  exactKeys(captured, ['manifestSha256', 'inputs', 'artifacts']);
  assert.equal(captured.manifestSha256, createHash('sha256').update(manifestBytes).digest('hex'));
  hashMap(captured.inputs, Object.keys(manifest.inputs));
  assert.deepEqual(captured.inputs, manifest.inputs);
  const artifactKeys = ['package.json', ...Object.values(manifest.packages).flatMap((pkg: any) => Object.keys(pkg.files).map(file => join(pkg.root, file)))];
  hashMap(captured.artifacts, artifactKeys);
  for (const [id, pkg] of Object.entries(manifest.packages) as Array<[string, any]>) {
    for (const [file, before] of Object.entries(pkg.files)) assert.equal(captured.artifacts[join(pkg.root, file)], manifest.files[`${id}/${file}`]?.after ?? before);
  }
  assert.equal(captured.artifacts['package.json'], createHash('sha256').update(await readFile(new URL('package.json', import.meta.url))).digest('hex'));
  const evidence = { schema: 1, contract: record.contract, home: record.home, node: record.node, nodeVersion: record.nodeVersion,
    nodeSha256: record.nodeSha256, main: record.main, entries, attestation: {
      manifestSha256: captured.manifestSha256, inputs: captured.inputs, artifacts: captured.artifacts,
    } };
  await writeFile(join(output, 'ios-installed-context-integrity.json'), JSON.stringify(evidence, null, 2) + '\n', { mode: 0o600 });
}

export async function retainIOSIntegrity(home: string, output: string): Promise<void> {
  try { await copyIntegrity(home, output); }
  catch { throw new Error('IOS_INTEGRITY_EVIDENCE_INVALID'); }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const [home, output, ...extra] = process.argv.slice(2);
    assert.ok(home && output && extra.length === 0);
    await retainIOSIntegrity(home, output);
  } catch {
    console.error('IOS_INTEGRITY_EVIDENCE_INVALID');
    process.exitCode = 1;
  }
}
