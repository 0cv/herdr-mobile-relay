import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdir, readFile, readdir, writeFile } from 'node:fs/promises';
import { dirname, join, relative } from 'node:path';
import ts from 'typescript';

export type Substitution = [string, string];

export const hash = (bytes: string | Buffer): string => createHash('sha256').update(bytes).digest('hex');

export function artifactReader(base: string) {
  const artifact = (name: string) => new URL(name, base);
  const load = async <T>(name: string): Promise<T> => JSON.parse(await readFile(artifact(name), 'utf8')) as T;
  return { artifact, load };
}

export async function configureHome(home: string, files: Array<[string, URL]>, freshMessage: string): Promise<void> {
  await mkdir(home, { recursive: true });
  assert.deepEqual(await readdir(home), [], freshMessage);
  for (const [name, source] of files) await writeFile(join(home, name), await readFile(source));
}

export function substitute(text: string, edits: Substitution[], label: string): string {
  let result = text;
  for (const [before, after] of edits) {
    assert.equal(result.split(before).length, 2, `Nonunique producer substitution: ${label}`);
    result = result.replace(before, after);
  }
  return result;
}

export function transpile(text: string, source: string, runtimePath: string): { code: string; map: string } {
  const result = ts.transpileModule(text, { fileName: source, compilerOptions: {
    target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext, sourceMap: true, inlineSources: true,
  } });
  assert.ok(result.sourceMapText);
  const map = JSON.parse(result.sourceMapText);
  map.sources = [relative(dirname(runtimePath), source)];
  return { code: result.outputText, map: JSON.stringify(map) };
}

export async function writeIntegrity(home: string, name: string, evidence: unknown): Promise<void> {
  await writeFile(join(home, name), JSON.stringify(evidence, null, 2) + '\n');
}
