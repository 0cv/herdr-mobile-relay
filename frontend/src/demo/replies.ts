import type { ConversationTool } from '../lib/types';
import { searchCheck } from './checks';
import { notesPath, resultsPath, sampleGitStatus, testsPath } from './samples';

function tool(name: string, key: string, value: string, output: string) {
  return { name, key, value, output };
}

const cases = {
  checks: {
    plan: 'I’ll check the results component and its fixtures.',
    tools: [
      tool('Read', 'path', resultsPath, 'Found the search reset and empty state.'),
      tool('Read', 'path', testsPath, 'Found focus and fallback coverage.'),
      tool('Bash', 'command', searchCheck.command, searchCheck.result),
    ],
    summary: ['The sample checks pass.', 'The search preview is ready for review.'],
  },
  change: {
    plan: 'I’ll announce when Clear search resets the query.',
    tools: [
      tool('Read', 'path', resultsPath, 'Clear restores focus but doesn’t announce the reset.'),
      tool('Edit', 'path', resultsPath, 'Sample edit: announce “Search cleared”.\nKeep the live region mounted across reset.'),
      tool('Bash', 'command', 'make test-search-reset', '2 passed: reset announcement, focus after clearing.'),
    ],
    summary: ['Clear search now announces the reset.', 'Focus still returns to the search box.'],
  },
  question: {
    plan: 'I’ll look up the rollout and fallback.',
    tools: [tool('Read', 'path', notesPath, 'Found the preview flag and fallback checklist.')],
    summary: ['The preview group can try /search?preview=1.', 'Turn off the flag to return to classic search.'],
  },
};

const styles = {
  codex: { plan: '• Plan: ', tool: '  └ ', result: '    ', summary: '• Summary: ' },
  claude: { plan: '● ', tool: '● ', result: '  ⎿  ', summary: '● ' },
  pi: { plan: 'Plan: ', tool: '', result: '  ', summary: 'Summary: ' },
  opencode: { plan: '# Task: ', tool: '→ ', result: '  ', summary: '# Review: ' },
};

type DemoKind = keyof typeof styles;

function statusReply(branch: string) {
  const status = sampleGitStatus(branch);
  return {
    plan: 'Here’s the sample checkout status.',
    tools: [tool('Bash', 'command', 'git status', `On branch ${status.branch}`)],
    summary: status.files.map(({ path }) => `Modified: ${path}`),
  };
}

export function sampleReply(agent: string | undefined, text: string, branch: string) {
  const normalized = text.trim().toLowerCase().replace(/\s+/g, ' ');
  const template = normalized === 'git status' ? statusReply(branch)
    : /\b(tests?|build|checks|lint|typecheck)\b/.test(normalized) ? cases.checks
    : /\b(fix|change|edit|implement|add|patch)\b/.test(normalized) ? cases.change : cases.question;
  const style = styles[agent as DemoKind] ?? styles.pi;
  const tools = template.tools.map(({ name, key, value, output }): ConversationTool => ({
    name, state: 'completed', input: JSON.stringify({ [key]: value }), output,
  }));
  return {
    lines: [
      'Scripted reply · no command or model ran',
      `${style.plan}${template.plan}`,
      ...template.tools.flatMap(({ name, value, output }) => [
        `${style.tool}${name === 'Bash' ? `$ ${value}` : `${name} ${value}`}`,
        ...output.split('\n').map((line) => `${style.result}${line}`),
      ]),
      `${style.summary}${template.summary[0]}`,
      ...template.summary.slice(1).map((line) => `${style.result}${line}`),
    ],
    text: `${template.summary.join(' ')} (Scripted.)`,
    tools,
  };
}

