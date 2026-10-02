import { createHash } from 'node:crypto';

export type TestCaseScope = 'outer' | 'ios-inner';

export type TestCaseIdentity = {
  id: string;
  name: string;
  scope: TestCaseScope;
};

export function stableTestIdentities(scope: TestCaseScope, names: readonly string[]): TestCaseIdentity[] {
  const occurrences = new Map<string, number>();
  return names.map((name) => {
    const occurrence = (occurrences.get(name) || 0) + 1;
    occurrences.set(name, occurrence);
    const digest = createHash('sha256').update(`${scope}\0${name}`).digest('hex').slice(0, 16);
    return { id: `${scope}:${digest}:${occurrence}`, name, scope };
  });
}
