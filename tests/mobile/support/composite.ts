import type { PolicyIssue } from './retention';

function indentation(line: string): number {
  return line.match(/^\s*/u)?.[0].length || 0;
}

function isStepBoundary(line: string, stepIndent: number): boolean {
  return /^\s*-\s/u.test(line) && indentation(line) <= stepIndent;
}

export function compositeIssues(source: string, filename = 'action.yml'): PolicyIssue[] {
  const lines = source.split(/\r?\n/u);
  if (!/^\s*using:\s*composite\s*$/mu.test(source)) return [];
  const issues: PolicyIssue[] = [];
  let run: { line: number; indent: number; stepIndent: number; shell: boolean } | undefined;
  const finish = (): void => {
    if (run && !run.shell) issues.push({ filename, line: run.line, message: 'composite run step is missing shell' });
    run = undefined;
  };
  lines.forEach((line, index) => {
    if (run && isStepBoundary(line, run.stepIndent)) finish();
    const runMatch = line.match(/^(?:(\s*)-\s+run:|(\s*)run:)\s*/u);
    if (runMatch) {
      let stepIndent = runMatch[1] === undefined ? -1 : runMatch[1].length;
      if (stepIndent < 0) {
        for (let cursor = index - 1; cursor >= 0; cursor -= 1) {
          if (/^\s*-\s/u.test(lines[cursor])) {
            stepIndent = indentation(lines[cursor]);
            break;
          }
        }
      }
      if (stepIndent >= 0) {
        finish();
        const indent = runMatch[1] === undefined ? runMatch[2].length : runMatch[1].length + 2;
        run = { line: index + 1, indent, stepIndent, shell: false };
      }
    }
    if (run) {
      const shellMatch = line.match(/^\s*shell:\s*(\S.*)$/u);
      if (shellMatch && indentation(line) === run.indent && shellMatch[1].trim()) run.shell = true;
    }
  });
  finish();
  return issues;
}
