export const rolloutCheck = {
  command: 'make test-rollout',
  cases: ['preview group', 'fallback to classic search'],
};

const searchTestResult = [
  '✓ preview flag opens new search',
  '✓ clearing returns focus to search',
  '✓ no matches suggest a shorter query',
  '✓ classic search remains available',
  '4 passed in the sample log',
];

export const searchCheck = { command: 'make test', result: searchTestResult.join('\n') };

export const sampleChecks = [
  searchCheck,
  { command: 'make lint', result: 'No warnings in the search fixtures.' },
  { command: 'make docs', result: '3 pages built; no broken internal links.' },
];
