import { describe, expect, it } from 'vitest';
import { createDemoResponder } from '../../src/demo/responder';
import { createDemoScene, createProjectInventory } from '../../src/demo/scene';
import { agentOpeningView } from '$lib/agent-view';
import { agentStatusGroup, approvalOptions, normalizeAgent, questionInteraction } from '$lib/agents';
import { parseApiError, relayProtocolError } from '$lib/protocol';
import { targetRefForAgent } from '$lib/resource-id';
import { normalizeAgentInventory } from '$lib/wire-normalizers';

const expectedInventory = {
  state: 'ready', error_code: '', message: '',
  last_attempt_at: 1_786_525_500, last_success_at: 1_786_525_500, stale: false,
};

describe('demo scene', () => {
  it('fixes computer, workspace, and agent identity, ordering and time', () => {
    const scene = createDemoScene();
    expect(scene.map(({ id, label, role }) => [id, label, role])).toEqual([
      ['demo-atlas', 'Atlas', 'controller'], ['demo-birch', 'Birch', 'reader'],
    ]);
    expect(scene.map(({ workspaces }) => workspaces.map(({ workspace_id, label, number }) => [workspace_id, label, number])))
      .toEqual([[['w1', 'Project', 1], ['w4', 'fix/one', 4]], [['w2', 'Docs', 1]]]);
    expect(scene.map(({ id, label, agents }) => agents.map((row) => {
      const target = targetRefForAgent(normalizeAgent(id, label, row))!;
      expect(row.last_active_at).toBe(row.updated_at);
      return [
        target.relay_id, target.server_session_id, target.pane_id, target.terminal_id, target.generation,
        target.agent_session_id, row.workspace_id, row.tab_id, row.tab_label, row.pane_revision, row.activity_seq,
        new Date(Number(row.updated_at)).toISOString(),
      ];
    }))).toEqual([
      [
        ['demo-atlas', 'atlas-herdr', 'w4:p1', 'atlas-review-terminal', 1, 'atlas-review-session',
          'w4', 'w4:t1', 'Patch review', 3, 3, '2026-08-12T09:05:00.000Z'],
        ['demo-atlas', 'atlas-herdr', 'w1:p2', 'atlas-plan-terminal', 1, 'atlas-plan-session',
          'w1', 'w1:t2', 'Release plan', 2, 2, '2026-08-12T09:04:00.000Z'],
        ['demo-atlas', 'atlas-herdr', 'w1:p1', 'atlas-notes-terminal', 1, 'atlas-notes-session',
          'w1', 'w1:t1', 'Release notes', 1, 1, '2026-08-12T09:03:00.000Z'],
      ],
      [
        ['demo-birch', 'birch-herdr', 'w2:p1', 'birch-docs-terminal', 1, 'birch-docs-session',
          'w2', 'w2:t1', 'Docs preview', 1, 1, '2026-08-12T09:02:00.000Z'],
      ],
    ]);
  });

  it('gives Atlas distinct approval and question agents, and a ready agent', () => {
    const [atlas] = createDemoScene();
    const [approval, question, ready] = atlas.agents.map((row) => normalizeAgent(
      atlas.id, atlas.label, row, atlas.capabilities.includes('attention_classification'),
    ));
    expect([approval, question, ready].map(agentStatusGroup)).toEqual(['blocked', 'blocked', 'ready']);
    expect(approval).toMatchObject({
      event_id: 'atlas-review-approval', attention_kind: 'approval',
      prompt: 'Allow the sample agent to run the test suite?', command: 'make test',
      approval_fingerprint: 'atlas-review-tests',
    });
    expect(approvalOptions(approval)).toEqual(['Allow once', 'Decline']);
    expect(atlas.agents[0]).not.toHaveProperty('interaction');
    expect(question.event_id).toBe('atlas-release-question');
    expect(questionInteraction(question)).toMatchObject({
      id: 'atlas-release-rollout', kind: 'single_select', question: 'How should we roll out the new search page?',
      options: [{ index: 0, label: 'Preview first' }, { index: 1, label: 'All at once' }],
      question_index: 1, question_total: 1,
    });
    expect(atlas.agents[1]).not.toHaveProperty('options');
    expect(atlas.agents[1]).not.toHaveProperty('approval_fingerprint');
    expect(ready.cwd).toBe('/work/project');
  });

  it('gives Birch a terminal-only agent without workspace inspection', () => {
    const [, birch] = createDemoScene();
    const docs = normalizeAgent(birch.id, birch.label, birch.agents[0]);
    expect(birch.capabilities).toEqual([]);
    expect(docs.conversation_history_available).toBe(false);
    const connected = {
      status: 'connected' as const,
      capabilities: birch.capabilities,
      inventory: normalizeAgentInventory(expectedInventory),
    };
    expect(agentOpeningView(docs, connected, 'conversation', {})).toMatchObject({ view: 'terminal' });
  });

  it('keeps consecutive initializations and the shared project inventory independent', () => {
    const first = createDemoScene();
    const second = createDemoScene();
    const pristine = structuredClone(second);
    expect(first).toEqual(pristine);

    const [atlas, birch] = first;
    atlas.capabilities.pop();
    atlas.agents[0].options!.pop();
    atlas.agents[1].interaction!.options[0].label = 'Changed';
    atlas.agents[1].interaction!.other!.hidden = false;
    atlas.workspaces[0].worktree!.repo_root = '/changed';
    birch.agents.pop();
    birch.workspaces[0].label = 'Changed';
    first.reverse();

    const shared = createProjectInventory();
    expect(shared.project.worktree.repo_root).toBe('/work/project');
    shared.linked.worktree.repo_name = 'Changed';
    expect(second).toEqual(pristine);
    expect(createDemoScene()).toEqual(pristine);
  });
});

describe('demo responder', () => {
  it.each(createDemoScene())('serves $label handshake, inventory and pongs on the relay wire', (computer) => {
    const relay = createDemoResponder(computer.id);
    const [handshake, ...inventory] = relay.initialMessages();
    if (handshake.type !== 'push_config') throw new Error('Expected push_config first');
    expect(handshake).toEqual({
      type: 'push_config', protocol: 3, host: computer.label, home: '/home/demo',
      capabilities: computer.capabilities, inventory: expectedInventory,
    });
    expect(relayProtocolError(handshake)).toBe('');
    expect(inventory).toEqual([
      { type: 'agents', agents: computer.agents },
      { type: 'workspaces', workspaces: computer.workspaces },
    ]);
    expect(relay.respond({ type: 'refresh_agents' })).toEqual([
      { type: 'inventory_status', ...expectedInventory }, ...inventory,
    ]);
    expect(relay.respond({ type: 'ping' })).toEqual([{ type: 'pong' }]);
  });

  it('isolates initial and refresh replies from other replies and responders', () => {
    const relay = createDemoResponder('demo-atlas');
    const other = createDemoResponder('demo-atlas');
    const initial = relay.initialMessages();
    const refresh = relay.respond({ type: 'refresh_agents' });
    const expectedInitial = structuredClone(initial);
    const expectedRefresh = structuredClone(refresh);

    for (const message of initial) {
      if (message.type === 'push_config') {
        message.capabilities.pop();
        message.inventory.last_success_at = 0;
      }
      if (message.type === 'agents') message.agents[1].interaction!.options.pop();
      if (message.type === 'workspaces') message.workspaces[0].worktree!.repo_name = 'Changed';
    }
    expect(refresh).toEqual(expectedRefresh);
    for (const message of refresh) {
      if (message.type === 'agents') message.agents[0].options!.pop();
      if (message.type === 'workspaces') message.workspaces.reverse();
    }
    expect(relay.respond({ type: 'refresh_agents' })).toEqual(expectedRefresh);
    expect(relay.initialMessages()).toEqual(expectedInitial);
    expect(other.initialMessages()).toEqual(expectedInitial);
  });

  it.each(['demo-atlas', 'demo-birch'])('rejects unsupported actions without changing %s', (id) => {
    const relay = createDemoResponder(id);
    const initial = relay.initialMessages();
    for (const type of ['respond', 'answer_question', 'submit_prompt', 'workspace_file', 'get_conversation_history', 'read_pane', 'not_a_command']) {
      expect(relay.respond({ type, request_id: `request-${type}`, text: 'Not part of the reply' })).toEqual([{
        type: 'error', request_id: `request-${type}`, error: { code: 'unknown_action' },
        detail: 'This action is not available in the demo.',
      }]);
    }
    const [rejection] = relay.respond({ type: 'watch_pane' });
    if (rejection.type !== 'error') throw new Error('Expected a wire error');
    expect(rejection).not.toHaveProperty('request_id');
    expect(parseApiError(rejection.error)).toEqual({ code: 'unknown_action' });
    expect(relay.initialMessages()).toEqual(initial);
  });

  it('refuses unknown computers instead of returning a default computer', () => {
    expect(() => createDemoResponder('unknown')).toThrow('Unknown demo computer.');
  });
});
