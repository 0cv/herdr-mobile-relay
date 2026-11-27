import type { DeviceRole } from '../lib/device-auth';
import type { Agent, AgentProfile, ConversationEntry, RelayWorkspace, TargetRef } from '../lib/types';
import { rolloutCheck } from './checks';

export const demoEpochKey = 'herdr-demo-epoch';
export const minuteMs = 60_000;

export type DemoAgent = Partial<Agent> & Required<TargetRef> & { pane_revision: number; activity_seq: number };
type DemoWorkspace = Omit<RelayWorkspace, 'relay_id' | 'relay_label'>;

export interface DemoComputer {
  id: string;
  label: string;
  role: DeviceRole;
  home: string;
  capabilities: string[];
  agents: DemoAgent[];
  workspaces: DemoWorkspace[];
}

export const demoComputers = [
  { id: 'demo-atlas', label: 'Atlas', role: 'controller' },
  { id: 'demo-birch', label: 'Birch', role: 'reader' },
] as const;

export const demoProfiles = [
  { id: 'codex', label: 'Codex' },
  { id: 'claude', label: 'Claude Code' },
  { id: 'pi', label: 'Pi' },
  { id: 'opencode', label: 'OpenCode' },
] as const satisfies readonly AgentProfile[];

export function sampleDeviceIds(relayId: string) {
  return { deviceId: `${relayId}-device`, credentialId: `${relayId}-credential` };
}

export function createProjectInventory() {
  return {
    project: {
      workspace_id: 'w1', number: 1, label: 'Project', pane_count: 1, tab_count: 1,
      cwd: '/work/project',
      worktree: {
        repo_key: 'repo', repo_name: 'project', repo_root: '/work/project',
        checkout_path: '/work/project', is_linked_worktree: false,
      },
    },
    linked: {
      workspace_id: 'w4', number: 4, label: 'fix/one', pane_count: 1, tab_count: 1,
      cwd: '/work/worktrees/project/fix-one',
      worktree: {
        repo_key: 'repo', repo_name: 'project', repo_root: '/work/project',
        checkout_path: '/work/worktrees/project/fix-one', is_linked_worktree: true,
      },
    },
    projectAgent: {
      pane_id: 'w1:p1', workspace_id: 'w1', tab_id: 'w1:t1', tab_number: 1,
      cwd: '/work/project', agent: 'codex',
    },
  };
}

export function createDemoConversation(epoch: number): ConversationEntry[] {
  const at = (minutesAgo: number) => new Date(epoch - minutesAgo * minuteMs).toISOString();
  return [
    {
      id: 'notes-1', role: 'user', timestamp: at(32),
      text: 'Review the search rollout plan',
    },
    {
      id: 'notes-2', role: 'assistant', timestamp: at(31),
      text: 'The notes assume everyone gets search at once. Let’s start with a preview group.',
      tools: [{
        id: 'notes-read', name: 'Read', state: 'completed',
        input: JSON.stringify({ path: 'docs/release-notes.md' }),
        output: 'The draft calls for a general rollout.',
      }],
    },
    {
      id: 'notes-3', role: 'user', timestamp: at(24),
      text: 'Draft a note for the search preview',
    },
    {
      id: 'notes-4', role: 'assistant', timestamp: at(23),
      text: 'I added the preview-first rollout note. Use the preview flag:\n\n```typescript\nconst previewUrl = "/search?preview=1";\n```',
      tools: [{
        id: 'notes-edit', name: 'Edit', state: 'completed',
        input: JSON.stringify({ path: 'docs/release-notes.md', before: 'Roll out to everyone.', after: 'Start with the search preview group.' }),
        output: 'Updated the sample release notes.',
      }],
    },
    {
      id: 'notes-5', role: 'user', timestamp: at(17),
      text: 'Verify the rollout before review',
    },
    {
      id: 'notes-6', role: 'assistant', timestamp: at(16),
      text: 'The preview flag and fallback checks passed in the sample log. The note is ready for review.',
      tools: [{
        id: 'notes-check', name: 'Bash', state: 'completed',
        input: JSON.stringify({ command: rolloutCheck.command }),
        output: `${rolloutCheck.cases.length} passed: ${rolloutCheck.cases.join(', ')}.`,
      }],
    },
  ];
}

export function createDemoScene(epoch: number): DemoComputer[] {
  const activity = (minutesAgo: number) => {
    const timestamp = epoch - minutesAgo * minuteMs;
    return { updated_at: timestamp, last_active_at: timestamp };
  };
  const project = '/home/demo/project';
  const linked = '/home/demo/project-fix-one';
  const search = '/home/demo/search';
  const docs = '/home/demo/docs';
  const examples = '/home/demo/examples';
  const repo = { repo_key: 'atlas-project', repo_name: 'project', repo_root: project };
  return [
    {
      ...demoComputers[0],
      home: '/home/demo',
      capabilities: [
        'attention_classification', 'conversation_history', 'manual_terminal_input', 'agent_launch',
        'directory_browser', 'workspace_inspection', 'invitation_qr', 'device_management', 'paging_keys',
      ],
      workspaces: [
        {
          workspace_id: 'w1', number: 1, label: 'Project', pane_count: 2, tab_count: 2,
          focused: true, active_tab_id: 'w1:t2', agent_status: 'blocked', cwd: project,
          worktree: { ...repo, checkout_path: project, is_linked_worktree: false },
        },
        {
          workspace_id: 'w4', number: 4, label: 'fix/one', pane_count: 2, tab_count: 2,
          focused: false, active_tab_id: 'w4:t1', agent_status: 'blocked', cwd: linked,
          worktree: { ...repo, checkout_path: linked, is_linked_worktree: true },
        },
        {
          workspace_id: 'w5', number: 5, label: 'Search', pane_count: 1, tab_count: 1,
          focused: false, active_tab_id: 'w5:t1', agent_status: 'working', cwd: search,
        },
      ],
      agents: [
        {
          server_session_id: 'atlas-herdr', pane_id: 'w4:p1', terminal_id: 'atlas-review-terminal',
          generation: 1, agent_session_id: 'atlas-review-session',
          workspace_id: 'w4', cwd: linked,
          tab_id: 'w4:t1', tab_number: 1, tab_order: 1, tab_label: 'Patch review',
          agent: 'codex', project: 'Patch review', status: 'blocked',
          pane_revision: 3, activity_seq: 3, ...activity(2),
          event_id: 'atlas-review-approval', attention_kind: 'approval',
          prompt: 'Allow the sample agent to run the test suite?', command: 'make test',
          approval_fingerprint: 'atlas-review-tests', options: ['Allow once', 'Decline'],
        },
        {
          server_session_id: 'atlas-herdr', pane_id: 'w1:p2', terminal_id: 'atlas-plan-terminal',
          generation: 1, agent_session_id: 'atlas-plan-session',
          workspace_id: 'w1', cwd: project,
          tab_id: 'w1:t2', tab_number: 2, tab_order: 2, tab_label: 'Release plan',
          agent: 'claude', project: 'Release plan', status: 'blocked',
          pane_revision: 2, activity_seq: 2, ...activity(4),
          event_id: 'atlas-release-question', attention_kind: 'question',
          interaction: {
            id: 'atlas-release-rollout', kind: 'single_select', question: 'How should we roll out the new search page?',
            options: [
              { index: 0, label: 'Preview first' },
              { index: 1, label: 'All at once' },
            ],
            other: { hidden: true }, submit_label: 'Choose rollout', question_index: 1, question_total: 1,
          },
        },
        {
          server_session_id: 'atlas-herdr', pane_id: 'w1:p1', terminal_id: 'atlas-notes-terminal',
          generation: 1, agent_session_id: 'atlas-notes-session',
          workspace_id: 'w1', cwd: project,
          tab_id: 'w1:t1', tab_number: 1, tab_order: 1, tab_label: 'Release notes',
          agent: 'pi', project: 'Release notes', status: 'idle', conversation_history_available: true,
          pane_revision: 1, activity_seq: 1, ...activity(16),
        },
        {
          server_session_id: 'atlas-herdr', pane_id: 'w5:p1', terminal_id: 'atlas-search-terminal',
          generation: 1, agent_session_id: 'atlas-search-session',
          workspace_id: 'w5', cwd: search,
          tab_id: 'w5:t1', tab_number: 1, tab_order: 1, tab_label: 'Search polish',
          agent: 'opencode', project: 'Search polish', status: 'working',
          pane_revision: 1, activity_seq: 1, ...activity(9),
        },
        {
          server_session_id: 'atlas-herdr', pane_id: 'w4:p2', terminal_id: 'atlas-fixtures-terminal',
          generation: 1, agent_session_id: 'atlas-fixtures-session',
          workspace_id: 'w4', cwd: linked,
          tab_id: 'w4:t2', tab_number: 2, tab_order: 2, tab_label: 'Fixture checks',
          agent: 'pi', project: 'Fixture checks', status: 'done',
          pane_revision: 1, activity_seq: 1, ...activity(32),
        },
      ],
    },
    {
      ...demoComputers[1],
      home: '/home/demo',
      capabilities: ['device_management'],
      workspaces: [
        {
          workspace_id: 'w2', number: 1, label: 'Docs', pane_count: 1, tab_count: 1,
          focused: true, active_tab_id: 'w2:t1', agent_status: 'working', cwd: docs,
        },
        {
          workspace_id: 'w6', number: 6, label: 'Examples', pane_count: 1, tab_count: 1,
          focused: false, active_tab_id: 'w6:t1', agent_status: 'idle', cwd: examples,
        },
      ],
      agents: [
        {
          server_session_id: 'birch-herdr', pane_id: 'w2:p1', terminal_id: 'birch-docs-terminal',
          generation: 1, agent_session_id: 'birch-docs-session',
          workspace_id: 'w2', cwd: docs,
          tab_id: 'w2:t1', tab_number: 1, tab_order: 1, tab_label: 'Docs preview',
          agent: 'claude', project: 'Docs preview', status: 'working', conversation_history_available: false,
          pane_revision: 1, activity_seq: 1, ...activity(65),
        },
        {
          server_session_id: 'birch-herdr', pane_id: 'w6:p1', terminal_id: 'birch-examples-terminal',
          generation: 1, agent_session_id: 'birch-examples-session',
          workspace_id: 'w6', cwd: examples,
          tab_id: 'w6:t1', tab_number: 1, tab_order: 1, tab_label: 'Example index',
          agent: 'codex', project: 'Example index', status: 'idle',
          pane_revision: 1, activity_seq: 1, ...activity(90),
        },
      ],
    },
  ];
}
