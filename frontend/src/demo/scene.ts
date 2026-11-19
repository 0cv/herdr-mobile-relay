import type { DeviceRole } from '../lib/device-auth';
import type { Agent, RelayWorkspace, TargetRef } from '../lib/types';

export const DEMO_TIME_MS = 1_786_525_500_000;

type DemoAgent = Partial<Agent> & Required<TargetRef>;
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

export function createDemoScene(): DemoComputer[] {
  const { project, linked, projectAgent } = createProjectInventory();
  return [
    {
      id: 'demo-atlas',
      label: 'Atlas',
      role: 'controller',
      home: '/home/demo',
      capabilities: ['attention_classification'],
      workspaces: [
        { ...project, pane_count: 2, tab_count: 2, focused: true, active_tab_id: 'w1:t2', agent_status: 'blocked' },
        { ...linked, focused: false, active_tab_id: 'w4:t1', agent_status: 'blocked' },
      ],
      agents: [
        {
          server_session_id: 'atlas-herdr', pane_id: 'w4:p1', terminal_id: 'atlas-review-terminal',
          generation: 1, agent_session_id: 'atlas-review-session',
          workspace_id: linked.workspace_id, cwd: linked.cwd,
          tab_id: 'w4:t1', tab_number: 1, tab_order: 1, tab_label: 'Patch review',
          agent: 'codex', project: 'Patch review', status: 'blocked',
          pane_revision: 3, activity_seq: 3, updated_at: DEMO_TIME_MS, last_active_at: DEMO_TIME_MS,
          event_id: 'atlas-review-approval', attention_kind: 'approval',
          prompt: 'Allow the sample agent to run the test suite?', command: 'make test',
          approval_fingerprint: 'atlas-review-tests', options: ['Allow once', 'Decline'],
        },
        {
          server_session_id: 'atlas-herdr', pane_id: 'w1:p2', terminal_id: 'atlas-plan-terminal',
          generation: 1, agent_session_id: 'atlas-plan-session',
          workspace_id: project.workspace_id, cwd: project.cwd,
          tab_id: 'w1:t2', tab_number: 2, tab_order: 2, tab_label: 'Release plan',
          agent: 'claude', project: 'Release plan', status: 'blocked',
          pane_revision: 2, activity_seq: 2, updated_at: DEMO_TIME_MS - 60_000, last_active_at: DEMO_TIME_MS - 60_000,
          event_id: 'atlas-release-question', attention_kind: 'question',
          interaction: {
            id: 'atlas-release-rollout', kind: 'single_select', question: 'How should we roll out the new search page?',
            options: [
              { index: 0, label: 'Preview first', description: 'Start with a small preview group.' },
              { index: 1, label: 'All at once', description: 'Include it in the next release for everyone.' },
            ],
            other: { hidden: true }, submit_label: 'Choose rollout', question_index: 1, question_total: 1,
          },
        },
        {
          ...projectAgent,
          server_session_id: 'atlas-herdr', terminal_id: 'atlas-notes-terminal',
          generation: 1, agent_session_id: 'atlas-notes-session',
          tab_label: 'Release notes', tab_order: 1, project: 'Release notes', status: 'idle',
          pane_revision: 1, activity_seq: 1, updated_at: DEMO_TIME_MS - 120_000, last_active_at: DEMO_TIME_MS - 120_000,
        },
      ],
    },
    {
      id: 'demo-birch',
      label: 'Birch',
      role: 'reader',
      home: '/home/demo',
      capabilities: [],
      workspaces: [{
        workspace_id: 'w2', number: 1, label: 'Docs', pane_count: 1, tab_count: 1,
        focused: true, active_tab_id: 'w2:t1', agent_status: 'working', cwd: '/work/docs',
      }],
      agents: [{
        server_session_id: 'birch-herdr', pane_id: 'w2:p1', terminal_id: 'birch-docs-terminal',
        generation: 1, agent_session_id: 'birch-docs-session',
        workspace_id: 'w2', cwd: '/work/docs',
        tab_id: 'w2:t1', tab_number: 1, tab_order: 1, tab_label: 'Docs preview',
        agent: 'claude', project: 'Docs preview', status: 'working', conversation_history_available: false,
        pane_revision: 1, activity_seq: 1, updated_at: DEMO_TIME_MS - 180_000, last_active_at: DEMO_TIME_MS - 180_000,
      }],
    },
  ];
}
