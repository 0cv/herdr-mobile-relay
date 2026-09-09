import { lstat, mkdir } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { resolve, sep } from 'node:path';

export const repositoryRoot = fileURLToPath(new URL('../../..', import.meta.url));

export function repositoryPath(value: string): string {
  return resolve(repositoryRoot, value);
}

function containsPath(parent: string, child: string): boolean {
  return child === parent || child.startsWith(`${parent}${sep}`);
}

export async function prepareOutput(value: string, inputs: string[] = []): Promise<string> {
  const output = resolve(value);
  if (output === repositoryRoot || containsPath(output, repositoryRoot)) {
    throw new Error(`MOBILE_OUTPUT: refusing repository or its parent as output: ${output}`);
  }
  for (const inputValue of inputs) {
    const input = resolve(inputValue);
    if (containsPath(output, input) || containsPath(input, output)) {
      throw new Error(`MOBILE_OUTPUT: output overlaps input: ${output}`);
    }
  }
  if (await lstat(output).catch(() => undefined)) {
    throw new Error(`MOBILE_OUTPUT: refusing to replace existing path: ${output}`);
  }
  await mkdir(output, { recursive: true, mode: 0o700 });
  return output;
}
