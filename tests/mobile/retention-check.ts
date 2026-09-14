import { readdir, readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { retentionIssues } from './support/retention';

async function yamlFiles(root: string): Promise<string[]> {
  const entries = await readdir(root, { withFileTypes: true });
  const files: string[] = [];
  for (const entry of entries) {
    const path = join(root, entry.name);
    if (entry.isDirectory()) files.push(...await yamlFiles(path));
    else if (entry.isFile() && /\.ya?ml$/u.test(entry.name)) files.push(path);
  }
  return files;
}

async function main(): Promise<void> {
  const root = process.argv[2] || '.github';
  const files = await yamlFiles(root);
  const issues = [];
  for (const filename of files) issues.push(...retentionIssues(await readFile(filename, 'utf8'), filename));
  if (issues.length) {
    for (const issue of issues) process.stderr.write(`${issue.filename}:${issue.line}: ${issue.message}\n`);
    process.exitCode = 1;
  }
}

main().catch((error: unknown) => {
  process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
  process.exitCode = 1;
});
