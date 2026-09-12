import type { PolicyIssue } from './retention';

interface YAMLRuntime {
  YAML: {
    parse(source: string): unknown;
  };
}

function lineNumber(source: string, key: string, occurrence: number): number {
  const lines = source.split(/\r?\n/u);
  let seen = 0;
  for (const [index, line] of lines.entries()) {
    if (!new RegExp(`(?:^|[\\s-])${key}:`, 'u').test(line)) continue;
    if (seen === occurrence) return index + 1;
    seen += 1;
  }
  return 1;
}

function yamlRuntime(): YAMLRuntime | undefined {
  return (globalThis as unknown as { Bun?: YAMLRuntime }).Bun;
}

export function compositeIssues(source: string, filename = 'action.yml'): PolicyIssue[] {
  const runtime = yamlRuntime();
  if (!runtime) return [{ filename, line: 1, message: 'YAML parser is unavailable' }];
  let document: unknown;
  try {
    document = runtime.YAML.parse(source);
  } catch (error) {
    return [{ filename, line: 1, message: `invalid YAML: ${error instanceof Error ? error.message : String(error)}` }];
  }
  if (!document || typeof document !== 'object' || Array.isArray(document)) return [];
  const runs = (document as Record<string, unknown>).runs;
  if (!runs || typeof runs !== 'object' || Array.isArray(runs)) return [];
  const runsRecord = runs as Record<string, unknown>;
  if (runsRecord.using !== 'composite') return [];
  const steps = runsRecord.steps;
  if (!Array.isArray(steps)) return [{ filename, line: lineNumber(source, 'steps', 0), message: 'composite action is missing a steps array' }];
  const issues: PolicyIssue[] = [];
  let runOccurrence = 0;
  for (const step of steps) {
    if (!step || typeof step !== 'object' || Array.isArray(step)) {
      issues.push({ filename, line: lineNumber(source, 'steps', 0), message: 'composite step must be a mapping' });
      continue;
    }
    const stepRecord = step as Record<string, unknown>;
    if (!Object.prototype.hasOwnProperty.call(stepRecord, 'run')) continue;
    const line = lineNumber(source, 'run', runOccurrence);
    runOccurrence += 1;
    if (typeof stepRecord.shell !== 'string' || stepRecord.shell.trim() === '') {
      issues.push({ filename, line, message: 'composite run step is missing shell' });
    }
  }
  return issues;
}
