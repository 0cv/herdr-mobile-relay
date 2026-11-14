import { describe, expect, it } from 'vitest';
import { AGENT_KIND_ALIASES, agentKind, normalizeAgentName, type AgentKind } from '$lib/agents';

const NAMES_BY_KIND: Record<AgentKind, string[]> = {
  claude: ['claude', 'claudecode', 'Claude Code', 'claude-code', 'CLAUDE_CODE'],
  codex: ['codex', 'openaicodex', 'codexcli', 'OpenAI Codex', 'Codex CLI'],
  cursor: ['cursor', 'cursoragent', 'Cursor Agent', 'cursor-agent'],
  grok: ['grok', 'grokbuild', 'Grok Build', 'grok-build'],
  qoder: ['qoder', 'qodercli', 'Qoder CLI'],
  pi: ['pi', 'picodingagent', 'pi-coding-agent', 'PiCodingAgent'],
  omp: ['omp', 'ohmypi', 'ompquestion', 'omp-question', 'oh-my-pi', 'Oh My Pi', 'oh_my_pi'],
  kimi: ['kimi', 'kimicode', 'kimicli', 'kimicodecli', 'Kimi Code CLI', 'Kimi Code', 'kimi-code', 'kimi-cli'],
  opencode: ['opencode', 'Open Code', 'open-code'],
  omo: ['omo', 'ohmyopencode', 'Oh My OpenCode', 'oh-my-opencode'],
  hermes: ['hermes', 'hermesagent', 'Hermes Agent', 'hermes-agent'],
  agy: ['agy', 'antigravity', 'antigravitycli', 'Antigravity CLI', 'antigravity-cli'],
};

describe('agentKind', () => {
  it('resolves every alias to its kind', () => {
    const listed = new Set<string>();
    for (const [kind, names] of Object.entries(NAMES_BY_KIND)) {
      for (const name of names) {
        expect(agentKind(name), name).toBe(kind);
        listed.add(normalizeAgentName(name));
      }
    }
    expect([...AGENT_KIND_ALIASES.keys()].filter((alias) => !listed.has(alias))).toEqual([]);
  });

  it('rejects unknown names', () => {
    for (const name of ['', '  ', 'shell', 'claude-extra', 'my-claude', 'pi-extra', 'devin', undefined, null]) {
      expect(agentKind(name), String(name)).toBe('');
    }
  });
});
