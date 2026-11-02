import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { execFileSync, spawnSync } from 'node:child_process';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { retainIOSIntegrity } from '../ios-appium/evidence';

export const iosIntegrityEvidenceTests: Array<[string, () => Promise<void>]> = [
  ['mobile iOS captured integrity evidence is bounded, allowlisted and filesystem only after failure', async () => {
    const directory = await mkdtemp(join(tmpdir(), 'ios-integrity-evidence-'));
    try {
      const home = join(directory, 'home'), output = join(directory, 'output');
      await mkdir(home); await mkdir(output);
      const manifestBytes = await readFile(new URL('../ios-appium/producer-manifest.json', import.meta.url));
      const manifest = JSON.parse(manifestBytes.toString());
      const hash = (bytes: Buffer) => createHash('sha256').update(bytes).digest('hex');
      const artifacts: Record<string, string> = { 'package.json': hash(await readFile(new URL('../ios-appium/package.json', import.meta.url))) };
      for (const [id, pkg] of Object.entries(manifest.packages) as Array<[string, any]>) {
        for (const [file, digest] of Object.entries(pkg.files)) artifacts[join(pkg.root, file)] = manifest.files[`${id}/${file}`]?.after ?? digest;
      }
      const record = { home, node: '/synthetic/node', nodeVersion: '26.13.0', nodeSha256: 'a'.repeat(64),
        main: join(home, manifest.packages.appium.root, manifest.packages.appium.entry),
        entries: Object.fromEntries(Object.entries(manifest.packages).map(([id, pkg]: [string, any]) => [id, pathToFileURL(join(home, pkg.root, pkg.entry)).href])),
        contract: 'herdr-installed-contexts-v1', selected: { name: 'appium-xcuitest-driver', version: '12.12.4', installPath: join(home, manifest.packages.xcuitest.root), mainClass: 'XCUITestDriver' },
        attestation: { manifestSha256: hash(manifestBytes), inputs: manifest.inputs, artifacts } };
      const input = join(home, 'installed-context-integrity.json');
      await writeFile(input, JSON.stringify(record));
      await retainIOSIntegrity(home, output);
      const retained = JSON.parse(await readFile(join(output, 'ios-installed-context-integrity.json'), 'utf8'));
      assert.deepEqual(retained.attestation, record.attestation);
      assert.equal(retained.nodeSha256, record.nodeSha256);
      assert.equal(retained.selected, undefined);
      const helper = new URL('../ios-appium/evidence.ts', import.meta.url).pathname;
      const sentinel = 'PRIVATE-URL-https://secret.invalid/XML-page-label-error-body-token';
      for (const mode of ['missing', 'oversize', 'malformed', 'extra-private-key', 'nested-private-key', 'invalid-digest'] as const) {
        await rm(input, { force: true });
        if (mode !== 'missing') {
          const changed = structuredClone(record) as any;
          if (mode === 'extra-private-key') changed.private = sentinel;
          if (mode === 'nested-private-key') changed.attestation.private = sentinel;
          if (mode === 'invalid-digest') changed.attestation.inputs['gate.ts'] = sentinel;
          await writeFile(input, mode === 'oversize' ? sentinel.repeat(5000) : mode === 'malformed' ? `{"${sentinel}` : JSON.stringify(changed));
        }
        await rm(join(output, 'ios-installed-context-integrity.json'), { force: true });
        await assert.rejects(() => retainIOSIntegrity(home, output), /^Error: IOS_INTEGRITY_EVIDENCE_INVALID$/u);
        const result = spawnSync(process.execPath, [helper, home, output], { encoding: 'utf8' });
        assert.equal(result.status, 1, mode);
        assert.equal(result.stdout, '', mode);
        assert.equal(result.stderr, 'IOS_INTEGRITY_EVIDENCE_INVALID\n', mode);
        await assert.rejects(() => readFile(join(output, 'ios-installed-context-integrity.json')));
      }
      await writeFile(input, JSON.stringify(record));
      assert.equal(execFileSync(process.execPath, [helper, home, output], { encoding: 'utf8' }), '');
      const source = await readFile(helper, 'utf8');
      assert.doesNotMatch(source, /child_process|gate\.ts|XCUITestDriver\(|fetch\(|AppiumClient/u);
    } finally { await rm(directory, { recursive: true, force: true }); }
  }],
];
