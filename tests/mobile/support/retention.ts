export interface PolicyIssue {
  filename: string;
  line: number;
  message: string;
}

function indentation(line: string): number {
  return line.match(/^\s*/u)?.[0].length || 0;
}

function stepIndentBefore(lines: string[], index: number, usesIndent: number): number | undefined {
  for (let cursor = index - 1; cursor >= 0; cursor -= 1) {
    const line = lines[cursor];
    if (!line.trim() || line.trimStart().startsWith('#')) continue;
    if (/^\s*-\s/u.test(line) && indentation(line) < usesIndent) return indentation(line);
  }
  return undefined;
}

function isStepBoundary(line: string, stepIndent: number): boolean {
  return /^\s*-\s/u.test(line) && indentation(line) <= stepIndent;
}

export function retentionIssues(source: string, filename = 'workflow.yml'): PolicyIssue[] {
  const lines = source.split(/\r?\n/u);
  const issues: PolicyIssue[] = [];
  let upload: { line: number; stepIndent: number; retention?: string } | undefined;

  const finish = (): void => {
    if (!upload) return;
    if (upload.retention !== '1') {
      issues.push({
        filename,
        line: upload.line,
        message: upload.retention === undefined
          ? 'upload-artifact is missing literal retention-days: 1'
          : `upload-artifact retention-days must be literal 1, found ${upload.retention}`,
      });
    }
    upload = undefined;
  };

  lines.forEach((line, index) => {
    if (upload && isStepBoundary(line, upload.stepIndent)) finish();
    const uploadMatch = line.match(/^\s*(?:-\s+)?uses:\s*actions\/upload-artifact@/u);
    if (uploadMatch) {
      const usesIndent = indentation(line);
      const stepIndent = /^\s*-\s+uses:/u.test(line)
        ? usesIndent
        : stepIndentBefore(lines, index, usesIndent);
      if (stepIndent !== undefined) {
        finish();
        upload = { line: index + 1, stepIndent };
      }
    }
    if (upload) {
      const retentionMatch = line.match(/^\s*retention-days:\s*(.*?)\s*$/u);
      if (retentionMatch && indentation(line) > upload.stepIndent) upload.retention = retentionMatch[1];
    }
  });
  finish();
  return issues;
}
