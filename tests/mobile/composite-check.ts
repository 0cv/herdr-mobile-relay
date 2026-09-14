import { readdir, readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { compositeIssues } from './support/composite';

async function actionFiles(root: string): Promise<string[]> {
  const entries = await readdir(root, { withFileTypes: true });
  const files: string[] = [];
  for (const entry of entries) {
    const path = join(root, entry.name);
    if (entry.isDirectory()) files.push(...await actionFiles(path));
    else if (entry.isFile() && /^action\.ya?ml$/u.test(entry.name)) files.push(path);
  }
  return files;
}

async function main(): Promise<void> {
  const root = process.argv[2] || '.github/actions';
  const files = await actionFiles(root);
  const issues = [];
  for (const filename of files) issues.push(...compositeIssues(await readFile(filename, 'utf8'), filename));
  if (issues.length) {
    for (const issue of issues) process.stderr.write(`${issue.filename}:${issue.line}: ${issue.message}\n`);
    process.exitCode = 1;
  }
}

main().catch((error: unknown) => {
  process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
  process.exitCode = 1;
});
