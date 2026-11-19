import { beforeEach, describe, expect, it } from 'vitest';
import { sampleReply } from '../../src/demo/replies';
import { createDemoResponder as responderAt } from '../../src/demo/responder';
import { createDemoConversation as conversationAt, createDemoScene as sceneAt, createProjectInventory, demoEpochKey, minuteMs, type DemoAgent } from '../../src/demo/scene';
import { createRelayTransport } from '../../src/demo/transport';
import { agentOpeningView } from '$lib/agent-view';
import { agentStatusGroup, approvalOptions, normalizeAgent, questionInteraction } from '$lib/agents';
import { parseApiError, relayProtocolError } from '$lib/protocol';
import { qrBitmap } from '$lib/qr';
import { targetRefForAgent } from '$lib/resource-id';
import { normalizeAgentInventory } from '$lib/wire-normalizers';

const epoch = 1_786_525_500_000;
const createDemoScene = () => sceneAt(epoch);
const createDemoConversation = () => conversationAt(epoch);
const createDemoResponder = (id: string) => responderAt(id, epoch);
const expectedInventory = {
  state: 'ready', error_code: '', message: '',
  last_attempt_at: epoch / 1_000, last_success_at: epoch / 1_000, stale: false,
};

describe('demo scene', () => {
  it('fixes computer, workspace, and agent identity, ordering and time', () => {
    const scene = createDemoScene();
    expect(scene.map(({ id, label, role }) => [id, label, role])).toEqual([
      ['demo-atlas', 'Atlas', 'controller'], ['demo-birch', 'Birch', 'reader'],
    ]);
    expect(scene.map(({ workspaces }) => workspaces.map(({ workspace_id, label, number }) => [workspace_id, label, number])))
      .toEqual([[['w1', 'Project', 1], ['w4', 'fix/one', 4], ['w5', 'Search', 5]], [['w2', 'Docs', 1], ['w6', 'Examples', 6]]]);
    expect(scene.map(({ id, label, agents }) => agents.map((row) => {
      const target = targetRefForAgent(normalizeAgent(id, label, row))!;
      expect(row.last_active_at).toBe(row.updated_at);
      return [
        target.relay_id, target.server_session_id, target.pane_id, target.terminal_id, target.generation,
        target.agent_session_id, row.workspace_id, row.tab_id, row.tab_label, row.pane_revision, row.activity_seq,
        (epoch - Number(row.updated_at)) / minuteMs,
      ];
    }))).toEqual([
      [
        ['demo-atlas', 'atlas-herdr', 'w4:p1', 'atlas-review-terminal', 1, 'atlas-review-session',
          'w4', 'w4:t1', 'Patch review', 3, 3, 2],
        ['demo-atlas', 'atlas-herdr', 'w1:p2', 'atlas-plan-terminal', 1, 'atlas-plan-session',
          'w1', 'w1:t2', 'Release plan', 2, 2, 4],
        ['demo-atlas', 'atlas-herdr', 'w1:p1', 'atlas-notes-terminal', 1, 'atlas-notes-session',
          'w1', 'w1:t1', 'Release notes', 1, 1, 16],
        ['demo-atlas', 'atlas-herdr', 'w5:p1', 'atlas-search-terminal', 1, 'atlas-search-session',
          'w5', 'w5:t1', 'Search polish', 1, 1, 9],
        ['demo-atlas', 'atlas-herdr', 'w4:p2', 'atlas-fixtures-terminal', 1, 'atlas-fixtures-session',
          'w4', 'w4:t2', 'Fixture checks', 1, 1, 32],
      ],
      [
        ['demo-birch', 'birch-herdr', 'w2:p1', 'birch-docs-terminal', 1, 'birch-docs-session',
          'w2', 'w2:t1', 'Docs preview', 1, 1, 65],
        ['demo-birch', 'birch-herdr', 'w6:p1', 'birch-examples-terminal', 1, 'birch-examples-session',
          'w6', 'w6:t1', 'Example index', 1, 1, 90],
      ],
    ]);
  });

  it('gives Atlas distinct waiting, ready, working and done agents', () => {
    const [atlas] = createDemoScene();
    const agents = atlas.agents.map((row) => normalizeAgent(
      atlas.id, atlas.label, row, atlas.capabilities.includes('attention_classification'),
    ));
    const [approval, question, ready] = agents;
    expect(agents.map(agentStatusGroup)).toEqual(['blocked', 'blocked', 'ready', 'working', 'done']);
    expect(agents.map(({ agent }) => agent)).toEqual(['codex', 'claude', 'pi', 'opencode', 'pi']);
    const [project, worktree] = atlas.workspaces;
    expect(project.worktree).toMatchObject({ repo_root: '/home/demo/project', checkout_path: project.cwd, is_linked_worktree: false });
    expect(worktree.worktree).toMatchObject({
      repo_key: project.worktree!.repo_key, repo_root: project.cwd,
      checkout_path: '/home/demo/project-fix-one', is_linked_worktree: true,
    });
    expect(atlas.workspaces.map(({ pane_count, tab_count, active_tab_id }) => [pane_count, tab_count, active_tab_id]))
      .toEqual([[2, 2, 'w1:t2'], [2, 2, 'w4:t1'], [1, 1, 'w5:t1']]);
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
    expect(ready.cwd).toBe('/home/demo/project');
  });

  it('gives Birch a terminal-only agent without workspace inspection', () => {
    const [, birch] = createDemoScene();
    const docs = normalizeAgent(birch.id, birch.label, birch.agents[0]);
    expect(birch.capabilities).toEqual(['device_management']);
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

function paneCommand(relay_id: string, { pane_id, server_session_id, terminal_id, generation, agent_session_id }: DemoAgent) {
  return { request_id: 'demo-request', pane_id, server_session_id, target: { relay_id, pane_id, server_session_id, terminal_id, generation, agent_session_id } };
}

function readPane(relay: ReturnType<typeof createDemoResponder>, pane: ReturnType<typeof paneCommand>) {
  const [reply] = relay.respond({ ...pane, type: 'read_pane', lines: 100 });
  if (reply.type !== 'pane_content') throw new Error('Expected pane content');
  return reply;
}

describe('demo replies', () => {
  it.each([
    [' RUN the search tests\n', ['Read', 'Read', 'Bash'], '4 passed in the sample log', 'make test'],
    ['fix the empty state', ['Read', 'Edit', 'Bash'], '2 passed: reset announcement, focus after clearing.', 'make test-search-reset'],
    ['how does the preview work?', ['Read'], 'Turn off the flag to return to classic search.', null],
    ['git status', ['Bash'], 'Modified: src/search/Results.svelte', null],
    ['fix and run tests', ['Read', 'Read', 'Bash'], 'The sample checks pass.', null],
  ] as const)('chooses fixed steps for %s, rendered in each agent’s style', (text, toolNames, result, command) => {
    const replies = ['codex', 'claude', 'pi', 'opencode'].map((kind) => {
      const reply = sampleReply(kind, text, 'main');
      expect(reply).toEqual(sampleReply(kind, text.trim().toLowerCase().replace(/\s+/g, ' '), 'main'));
      expect(reply.tools.map(({ name }) => name)).toEqual(toolNames);
      expect(reply.lines.join('\n')).toContain(result);
      if (command) expect(reply.tools.at(-1)).toMatchObject({ input: JSON.stringify({ command }), output: expect.stringContaining(result) });
      expect(reply.lines[0]).toBe('Scripted reply · no command or model ran');
      return reply.lines[1];
    });
    expect(new Set(replies).size).toBe(4);
  });
});

describe('demo responder', () => {
  it('keeps the seeded rollout check distinct from a scripted full search run', () => {
    const [atlas] = createDemoScene();
    const relay = createDemoResponder(atlas.id);
    const notes = paneCommand(atlas.id, atlas.agents[2]);
    const seeded = createDemoConversation().at(-1)?.tools?.[0];
    expect(seeded).toMatchObject({
      name: 'Bash', input: '{"command":"make test-rollout"}',
      output: '2 passed: preview group, fallback to classic search.',
    });
    expect(readPane(relay, notes).content).toContain('$ make test-rollout\n  loading search rollout fixtures\n  pass  preview group\n  pass  fallback to classic search\n  2 passed in the sample log');
    relay.respond({ ...notes, type: 'submit_prompt', text: 'Run the search tests' });
    const log = readPane(relay, notes).content;
    expect(log).toContain('$ make test\n  ✓ preview flag opens new search');
    expect(log).toContain('4 passed in the sample log');
  });

  it('uses one epoch for inventory, agents, device dates, history and mutations', () => {
    const snapshots = [epoch, Date.UTC(2031, 3, 6, 14, 8, 0, 321)].map((visit) => {
      const [atlas] = sceneAt(visit);
      const relay = responderAt(atlas.id, visit);
      expect(relay.initialMessages()[0].inventory.last_success_at).toBe(Math.floor(visit / 1_000));
      const notes = paneCommand(atlas.id, atlas.agents[2]);
      const prompt = { ...notes, type: 'submit_prompt', text: 'run the search tests' };
      const [result, update] = relay.respond(prompt);
      expect(result).toMatchObject({ ok: true, phase: 'completed' });
      expect(update).toMatchObject({ updated_at: visit + 1_000, last_active_at: visit + 1_000 });
      expect(relay.respond(prompt)).toMatchObject([{}, { updated_at: visit + 2_000 }]);
      const [devices] = relay.respond({ type: 'device_list', request_id: 'devices' });
      if (!('data' in devices)) throw new Error('Expected sample devices');
      const { paired_at, last_seen_at } = (devices.data.devices as Array<{ paired_at: string; last_seen_at: string }>)[0];
      const history = conversationAt(visit);
      expect(history.at(-1)?.timestamp).toBe(new Date(Number(atlas.agents[2].last_active_at)).toISOString());
      return {
        agents: sceneAt(visit).flatMap(({ agents }) => agents.map(({ updated_at }) => visit - Number(updated_at))),
        history: history.map(({ timestamp }) => visit - Date.parse(timestamp)),
        paired: visit - Date.parse(paired_at), seen: visit - Date.parse(last_seen_at),
        pane: readPane(relay, notes).content,
      };
    });
    expect(snapshots[0]).toEqual(snapshots[1]);
    expect(snapshots[0]).toMatchObject({ paired: 90 * minuteMs, seen: 2 * minuteMs });
  });

  it.each([
    ['demo-atlas', 'w4:p1', '• Explored'], ['demo-atlas', 'w1:p2', '● Proposed checklist'],
    ['demo-atlas', 'w1:p1', 'Edit docs/release-notes.md'], ['demo-atlas', 'w5:p1', '# Bash: check the search fixtures'],
    ['demo-atlas', 'w4:p2', 'Fixture review complete.'], ['demo-birch', 'w2:p1', '● Bash(make docs)'],
    ['demo-birch', 'w6:p1', '• Checked sample index'],
  ])('opens %s %s with a full transcript', (id, pane, signature) => {
    const computer = createDemoScene().find((entry) => entry.id === id)!;
    const agent = computer.agents.find(({ pane_id }) => pane_id === pane)!;
    const { content } = readPane(createDemoResponder(id), paneCommand(id, agent));
    const rows = content.split('\n');
    expect(rows.length).toBeGreaterThanOrEqual(30);
    expect(rows.length).toBeLessThanOrEqual(38);
    expect(rows.filter((row) => row.trim()).length).toBeGreaterThanOrEqual(23);
    expect(rows.every((row) => row.length <= (computer.role === 'reader' ? 34 : 60))).toBe(true);
    expect(content).toContain(signature);
    expect(content).toContain('Sample session · search rollout');
  });
  it.each(createDemoScene())('serves $label handshake, inventory and pongs on the relay wire', (computer) => {
    const relay = createDemoResponder(computer.id);
    const [handshake, ...announcements] = relay.initialMessages();
    const controller = computer.role === 'controller';
    expect(handshake).toEqual({
      type: 'push_config', protocol: 3, host: computer.label, home: '/home/demo',
      capabilities: computer.capabilities, inventory: expectedInventory,
      ...(controller ? { agent_profiles: [
        { id: 'codex', label: 'Codex' }, { id: 'claude', label: 'Claude Code' }, { id: 'pi', label: 'Pi' }, { id: 'opencode', label: 'OpenCode' },
      ] } : {}),
    });
    expect(relayProtocolError(handshake)).toBe('');
    expect(announcements).toEqual(controller ? [{
      type: 'speech_voices', cache_dir: '', engine_installed: false, languages: [], voices: [
        { language: 'en', name: 'English — Meadow', installed: false, bytes: 0, engine: '' },
        { language: 'fr', name: 'French — Willow', installed: false, bytes: 0, engine: '' },
      ],
    }] : []);
    expect(relay.respond({ type: 'refresh_agents' })).toEqual([
      { type: 'inventory_status', ...expectedInventory },
      { type: 'agents', agents: computer.agents },
      { type: 'workspaces', workspaces: computer.workspaces },
    ]);
    expect(relay.respond({ type: 'ping' })).toEqual([{ type: 'pong' }]);
    expect(relay.respond({ type: 'get_activity', limit: 500 })).toEqual([{ type: 'activity_history', activities: [] }]);
  });

  it('isolates handshake and refresh replies from other replies and responders', () => {
    const relay = createDemoResponder('demo-atlas');
    const other = createDemoResponder('demo-atlas');
    const initial = relay.initialMessages();
    const [handshake, voices] = initial;
    const refresh = relay.respond({ type: 'refresh_agents' });
    const expectedInitial = structuredClone(initial);
    const expectedRefresh = structuredClone(refresh);

    if (!voices) throw new Error('Expected Atlas sample voices');
    handshake.capabilities.pop();
    handshake.inventory.last_success_at = 0;
    handshake.agent_profiles?.pop();
    voices.voices.pop();
    for (const message of refresh) {
      if (message.type === 'agents') {
        message.agents[0].options!.pop();
        message.agents[1].interaction!.options.pop();
      }
      if (message.type === 'workspaces') {
        message.workspaces[0].worktree!.repo_name = 'Changed';
        message.workspaces.reverse();
      }
    }
    expect(relay.respond({ type: 'refresh_agents' })).toEqual(expectedRefresh);
    expect(other.respond({ type: 'refresh_agents' })).toEqual(expectedRefresh);
    expect(relay.initialMessages()).toEqual(expectedInitial);
    expect(other.initialMessages()).toEqual(expectedInitial);
  });

  it.each(['demo-atlas', 'demo-birch'])('rejects unsupported actions without changing %s', (id) => {
    const relay = createDemoResponder(id);
    const initial = relay.respond({ type: 'refresh_agents' });
    for (const type of [
      'navigate_question', 'clarify_question', 'get_conversation_updates', 'not_a_command',
      'create_device_invitation', 'rename_device', 'revoke_device', 'reset_devices',
      'workspace_create', 'workspace_rename', 'workspace_reorder', 'workspace_close', 'worktree_create', 'worktree_open', 'worktree_remove',
      'agent_stop', 'agent_clear', 'agent_rename', 'tab_reorder', 'speech_voices_list', 'speech_voice_install', 'speech_voice_remove', 'speak_text',
    ]) {
      expect(relay.respond({ type, request_id: `request-${type}`, text: 'Not part of the reply' })).toEqual([{
        type: 'error', request_id: `request-${type}`, error: { code: 'unknown_action' },
        detail: 'This action is not available in the demo.',
      }]);
    }
    const [rejection] = relay.respond({ type: 'watch_pane' });
    if (rejection.type !== 'error') throw new Error('Expected a wire error');
    expect(rejection).not.toHaveProperty('request_id');
    expect(parseApiError(rejection.error)).toEqual({ code: 'unknown_action' });
    expect(relay.respond({ type: 'refresh_agents' })).toEqual(initial);
  });

  it.each(createDemoScene())('correlates exact pane reads and checks acknowledgement on $label', ({ id, role, agents }) => {
    const relay = createDemoResponder(id);
    const { pane_id, server_session_id, terminal_id, generation, agent_session_id } = agents[0];
    const target = { relay_id: id, pane_id, server_session_id, terminal_id, generation, agent_session_id };
    const read = { type: 'read_pane', request_id: 'read-1', pane_id, server_session_id, target, lines: 1, format: 'ansi' };
    expect(relay.respond(read)).toEqual([{
      type: 'pane_content', request_id: 'read-1', pane_id, target, format: 'plain', no_echo: false,
      content: role === 'controller'
        ? 'Run make test before wrapping up? Waiting for approval.' : 'Building the sample docs preview…',
    }]);
    for (const mismatch of [{ relay_id: 'other' }, { server_session_id: 'other' }, { pane_id: 'other' },
      { terminal_id: 'other' }, { generation: 99 }, { agent_session_id: 'other' }]) {
      expect(relay.respond({ ...read, target: { ...target, ...mismatch } })).toMatchObject([{
        type: 'command_result', request_id: 'read-1', ok: false, phase: 'not_started',
      }]);
    }
    expect(relay.respond({ ...read, pane_id: 'other' })).toMatchObject([{ ok: false }]);
    expect(relay.respond({ ...read, lines: 0 })).toMatchObject([{ ok: false }]);
    expect(relay.respond({ ...read, type: 'acknowledge_pane', request_id: 'ack-1' })).toMatchObject([{
      type: 'command_result', action: 'acknowledge_pane', request_id: 'ack-1',
      ok: role === 'controller', phase: role === 'controller' ? 'completed' : 'not_started',
    }]);
  });

  it('appends sanitized prompts and records only the notes conversation', () => {
    const [atlas] = createDemoScene();
    const relay = createDemoResponder(atlas.id);
    const notes = paneCommand(atlas.id, atlas.agents[2]);
    const search = paneCommand(atlas.id, atlas.agents[3]);
    const history = { ...notes, type: 'get_conversation_history', cursor: '', limit: 20, observe: false };
    expect(relay.respond(history)).toEqual([{
      type: 'command_result', action: history.type, request_id: history.request_id, ok: true, phase: 'completed',
      data: { available: true, state: 'ready', mode: 'recent', source_revision: 'demo-notes', next_cursor: '', has_more: false, total: 6, entries: createDemoConversation() },
    }]);
    for (const badHistory of [{ cursor: 'stale' }, { observe: true }, { limit: 0 }, search]) {
      expect(relay.respond({ ...history, ...badHistory })).toMatchObject([{ ok: false, phase: 'not_started' }]);
    }
    const seeded = createDemoConversation();
    expect(seeded.map(({ role }) => role)).toEqual(['user', 'assistant', 'user', 'assistant', 'user', 'assistant']);
    expect(seeded.flatMap(({ tools }) => (tools ?? []).map(({ name, state }) => [name, state])))
      .toEqual([['Read', 'completed'], ['Edit', 'completed'], ['Bash', 'completed']]);
    const text = 'fix the empty state\n[example](https://preview.invalid) <b>sample</b>';
    const response = 'Clear search now announces the reset. Focus still returns to the search box. (Scripted.)';
    const searchBefore = readPane(relay, search);
    const send = { ...notes, type: 'submit_prompt', text: `\x1b[31m${text}\x1b[0m\x07` };
    const result = relay.respond(send);
    expect(result).toMatchObject([
      { action: 'submit_prompt', request_id: notes.request_id, ok: true, phase: 'completed' },
      { type: 'agent_update', pane_id: notes.pane_id, pane_revision: 2, activity_seq: 2, updated_at: epoch + 1_000 },
    ]);
    expect(createDemoResponder(atlas.id).respond(send)).toEqual(result);
    const after = readPane(relay, notes);
    expect(after.content).toContain(`You: ${text}\nScripted reply · no command or model ran`);
    expect(after.content).toContain('Edit src/search/Results.svelte\n  Sample edit: announce “Search cleared”.\n  Keep the live region mounted across reset.');
    expect(after.content.split(text)).toHaveLength(2);
    expect(after.content).not.toMatch(/[\x1b\x07]/);
    expect(readPane(relay, { ...notes, request_id: 'another-read' })).toEqual({ ...after, request_id: 'another-read' });
    expect(readPane(relay, search)).toEqual(searchBefore);
    expect(relay.respond({ ...search, type: 'submit_prompt', text: 'Search-only sample prompt' })[0]).toMatchObject({ ok: true });
    const timestamp = new Date(epoch + 1_000).toISOString();
    expect(relay.respond({ ...history, limit: 2 })).toMatchObject([{
      data: { total: 8, source_revision: 'demo-notes', entries: [
        { id: 'notes-7', role: 'user', text, timestamp },
        { id: 'notes-8', role: 'assistant', text: response, timestamp, tools: [
          { name: 'Read', state: 'completed', output: 'Clear restores focus but doesn’t announce the reset.' },
          { name: 'Edit', state: 'completed', input: '{"path":"src/search/Results.svelte"}', output: 'Sample edit: announce “Search cleared”.\nKeep the live region mounted across reset.' },
          { name: 'Bash', state: 'completed', input: '{"command":"make test-search-reset"}', output: '2 passed: reset announcement, focus after clearing.' },
        ] },
      ] },
    }]);
    const [page] = relay.respond(history);
    if (page.type !== 'command_result' || !('data' in page)) throw new Error('Expected history');
    (page.data.entries as unknown[]).pop();
    expect(relay.respond(history)).toMatchObject([{ data: { total: 8, entries: [...seeded, { id: 'notes-7' }, { id: 'notes-8' }] } }]);
  });

  it('echoes keys and stages literal text without answering a waiting agent', () => {
    const [atlas] = createDemoScene();
    const relay = createDemoResponder(atlas.id);
    const review = paneCommand(atlas.id, atlas.agents[0]);
    const inventory = relay.respond({ type: 'refresh_agents' });
    expect(relay.respond({ ...review, type: 'send_terminal_text', text: '  \x1b[31mliteral review text\x1b[0m  ' })).toMatchObject([{ ok: true, phase: 'completed' }]);
    expect(readPane(relay, review).content).toContain('Typed:   literal review text  \nDemo: Text received, waiting for Enter.');
    expect(readPane(relay, review).content).not.toContain('Keys: Enter');
    expect(relay.respond({ ...review, type: 'send_keys', keys: ['Tab'], activity_label: 'Tab' })).toMatchObject([{ ok: true, phase: 'completed' }]);
    expect(readPane(relay, review).content).toContain('Keys: Tab\nDemo response: Key input received');
    const unavailable = 'Sample checks are unavailable while this agent is waiting for a choice.';
    const committed = 'Sample text committed. The waiting choice is unchanged.';
    expect(relay.respond({ ...review, type: 'send_keys', keys: ['Down'] })).toMatchObject([
      { ok: true, phase: 'completed', data: { message: unavailable } },
    ]);
    expect(relay.respond({ ...review, type: 'send_keys', keys: ['Enter'], activity_label: 'Manual Enter' })).toMatchObject([
      { ok: true, phase: 'completed', data: { message: committed } },
    ]);
    expect(readPane(relay, review).content).toContain(`Keys: Down\n${unavailable}\n\nKeys: Enter\n${committed}`);
    expect(readPane(relay, review).content).not.toContain('› Sample check');
    const question = paneCommand(atlas.id, atlas.agents[1]);
    const questionBefore = readPane(relay, question);
    const interrupted = '^C · Sample step interrupted.';
    const useControls = 'Use the waiting controls to make a choice.';
    expect(relay.respond({ ...question, type: 'send_keys', keys: ['Up', 'ctrl+c', 'Enter'] })).toMatchObject([
      { ok: true, phase: 'completed', data: { message: `${unavailable} ${interrupted} ${useControls}` } },
    ]);
    expect(readPane(relay, question).content).toBe(`${questionBefore.content}\n\nKeys: Up, ctrl+c, Enter\n${unavailable}\n${interrupted}\n${useControls}`);
    expect(relay.respond({ type: 'refresh_agents' })).toEqual(inventory);
  });

  it('moves a per-terminal check cursor and cancels or commits staged text', () => {
    const [atlas] = createDemoScene();
    const relay = createDemoResponder(atlas.id);
    const notes = paneCommand(atlas.id, atlas.agents[2]);
    const search = paneCommand(atlas.id, atlas.agents[3]);
    const keys = (pane: typeof notes, ...keys: string[]) => relay.respond({ ...pane, type: 'send_keys', keys });
    const stage = (pane: typeof notes, text: string) => relay.respond({ ...pane, type: 'send_terminal_text', text });
    const before = readPane(relay, notes);
    expect(keys(search, 'Down', 'Right', 'Left', 'Up')).toMatchObject([{
      ok: true, phase: 'completed', data: { message: '› Sample check 1/3: make test › Sample check 2/3: make lint › Sample check 1/3: make test › Sample check 3/3: make docs' },
    }]);
    expect(readPane(relay, search).content).toContain('› Sample check 1/3: make test\n› Sample check 2/3: make lint\n› Sample check 1/3: make test\n› Sample check 3/3: make docs');
    expect(readPane(relay, notes)).toEqual(before);
    expect(keys(search, 'Enter')).toMatchObject([
      { action: 'send_keys', request_id: search.request_id, ok: true, phase: 'completed',
        data: { message: 'Scripted check · make docs — 3 pages built; no broken internal links.' } },
      { type: 'agent_update', pane_id: search.pane_id },
    ]);
    expect(readPane(relay, search).content).toContain('Scripted check · $ make docs\n3 pages built; no broken internal links.\nNo command ran.');
    stage(notes, 'fix the empty state');
    keys(notes, 'Down', 'Escape', 'Enter');
    expect(readPane(relay, notes).content).toContain('Sample input and check selection cleared.\nNo sample input.');
    stage(notes, 'git status');
    keys(notes, 'Up', 'ctrl+c', 'Enter');
    expect(readPane(relay, notes).content).toContain('^C · Sample step interrupted.\nNo sample input.');
    expect(readPane(relay, notes).content).not.toContain('Scripted reply');
    stage(notes, 'run the search ');
    stage(notes, 'tests');
    keys(notes, 'Down', 'Enter');
    expect(readPane(relay, notes).content).toContain('Keys: Down, Enter\n› Sample check 1/3: make test\nScripted reply · no command or model ran');
    expect(readPane(relay, notes).content).not.toContain('You: run the search tests');
    const [history] = relay.respond({ ...notes, type: 'get_conversation_history', cursor: '', limit: 2, observe: false });
    expect(history).toMatchObject({ data: { total: 8, entries: [
      { text: 'run the search tests', role: 'user' }, { role: 'assistant', tools: [{ name: 'Read' }, { name: 'Read' }, { name: 'Bash' }] },
    ] } });
  });

  it('rejects delivery and answers from a reader with a valid target', () => {
    const [, birch] = createDemoScene();
    const relay = createDemoResponder(birch.id);
    const pane = paneCommand(birch.id, birch.agents[0]);
    const before = readPane(relay, pane);
    for (const type of ['submit_prompt', 'send_keys', 'send_terminal_text', 'respond', 'answer_question']) {
      expect(relay.respond({ ...pane, type, text: 'Reader attempt', keys: ['Enter'] })).toEqual([{
        type: 'command_result', action: type, request_id: pane.request_id, ok: false,
        phase: 'not_started', error: 'This sample computer is read-only.',
      }]);
    }
    expect(readPane(relay, pane)).toEqual(before);
  });

  it.each([
    {
      index: 0, type: 'respond', outcome: 'Approval: Decline. Demo only; make test was not run.',
      choice: { index: 1, total: 2, choice: 'Decline', event_id: 'atlas-review-approval', approval_fingerprint: 'atlas-review-tests' },
      invalid: [{ index: 0 }, { total: 1 }, { choice: 'Unknown' }, { event_id: 'stale' }, { approval_fingerprint: 'stale' }],
    },
    {
      index: 1, type: 'answer_question', outcome: 'Rollout choice: Preview first. Demo only; nothing was deployed.',
      choice: { interaction_id: 'atlas-release-rollout', selected_indices: [0], other_selected: false, other_text: '' },
      invalid: [{ interaction_id: 'stale' }, { selected_indices: [2] }, { selected_indices: [0, 1] }, { other_selected: true }, { other_text: 'not offered' }],
    },
  ])('validates $type, clears only its target and rejects replay', ({ index, type, choice, invalid, outcome }) => {
    const [atlas] = createDemoScene();
    const relay = createDemoResponder(atlas.id);
    const agent = atlas.agents[index];
    const pane = paneCommand(atlas.id, agent);
    const sibling = paneCommand(atlas.id, atlas.agents[1 - index]);
    const siblingBefore = readPane(relay, sibling);
    relay.respond({ ...pane, type: 'send_terminal_text', text: 'How does the preview work?' });
    const before = readPane(relay, pane);
    const command = { ...pane, type, source: 'App', ...choice };
    for (const mismatch of [...invalid, { target: { ...pane.target, generation: 2 } }, { source: 'other' }]) {
      expect(relay.respond({ ...command, ...mismatch })).toMatchObject([{ ok: false, phase: 'not_started' }]);
    }
    expect(readPane(relay, pane)).toEqual(before);
    const result = relay.respond(command);
    const cleared = { ...agent, status: 'working', pane_revision: agent.pane_revision + 1, activity_seq: agent.activity_seq + 1,
      updated_at: 1_786_525_501_000, last_active_at: 1_786_525_501_000 };
    for (const field of ['options', 'command', 'approval_fingerprint', 'interaction', 'attention_kind', 'event_id', 'prompt'] as const) {
      delete cleared[field];
    }
    expect(result).toEqual([
      ...(type === 'respond' ? [{ ...cleared, type: 'agent_update' }] : []),
      { type: 'command_result', action: type, request_id: pane.request_id, ok: true, phase: 'confirmed', data: {} },
    ]);
    const [, agents] = relay.respond({ type: 'refresh_agents' });
    expect(agents).toEqual({ type: 'agents', agents: atlas.agents.map((row, position) => position === index ? cleared : row) });
    expect(relay.respond(command)).toMatchObject([{ ok: false, phase: 'not_started' }]);
    expect(readPane(relay, pane).content).toBe(`${before.content}\n\n${outcome}`);
    expect(relay.respond({ ...pane, type: 'send_keys', keys: ['Down', 'Enter'] })).toMatchObject([
      { ok: true, phase: 'completed', data: { message: '› Sample check 1/3: make test Scripted check · make test — 4 passed in the sample log' } },
      { type: 'agent_update', pane_id: pane.pane_id },
    ]);
    const log = readPane(relay, pane).content;
    expect(log).toContain('› Sample check 1/3: make test\nScripted check · $ make test');
    expect(log).toContain('4 passed in the sample log\nNo command ran.');
    expect(log).not.toContain('Scripted reply');
    expect(readPane(relay, sibling)).toEqual(siblingBefore);
  });

  it('browses only sample folders and launches unique sample panes on Atlas', () => {
    const [atlas, birch] = createDemoScene();
    const relay = createDemoResponder(atlas.id);
    const reader = createDemoResponder(birch.id);
    const request_id = 'sample-launch';
    const browse = (path: unknown) => relay.respond({ type: 'list_directories', request_id, path });
    const directories = ['project', 'project-fix-one', 'search'].map((name) => ({ name, path: `/home/demo/${name}` }));
    expect(browse('')).toEqual([{
      type: 'command_result', action: 'list_directories', request_id, ok: true, phase: 'completed',
      data: { current: { path: atlas.home, label: 'Home' }, parent: '', directories },
    }]);
    expect(browse(directories[1].path)).toMatchObject([{
      data: { current: { path: directories[1].path, label: 'project-fix-one' }, parent: atlas.home, directories: [] },
    }]);
    expect(browse(`${atlas.home}/../project`)).toMatchObject([{ ok: false, phase: 'not_started' }]);
    const [listing] = browse('');
    if (!('data' in listing)) throw new Error('Expected directory listing');
    (listing.data.directories as unknown[]).pop();
    expect(browse('')).toMatchObject([{ data: { directories } }]);

    const refresh = () => relay.respond({ type: 'refresh_agents' });
    const initial = refresh();
    const command = { type: 'agent_start', request_id, profile_id: 'codex', name: 'sample-run', cwd: directories[1].path, workspace_id: 'w4', prompt: '\x1b[31mTry the preview\x1b[0m' };
    for (const invalid of [{ profile_id: 'shell' }, { name: 'bad name' }, { cwd: atlas.home }, { cwd: '/etc' }, { workspace_id: 'w2' }, { prompt: 2 }]) {
      expect(relay.respond({ ...command, ...invalid })).toMatchObject([{ ok: false, phase: 'not_started', request_id }]);
    }
    expect(reader.respond(command)).toMatchObject([{ ok: false, phase: 'not_started', request_id }]);
    expect(reader.respond({ type: 'list_directories', request_id, path: '' })).toMatchObject([{ ok: false }]);
    expect(refresh()).toEqual(initial);
    expect(relay.respond(command)).toEqual([{
      type: 'command_result', action: 'agent_start', request_id, ok: true, phase: 'completed', data: { pane_id: 'w4:demo-p1' },
    }]);
    expect(relay.respond({ ...command, workspace_id: '', profile_id: 'pi', name: 'search-run', cwd: directories[2].path, prompt: '' }))
      .toMatchObject([{ ok: true, data: { pane_id: 'w5:demo-p2' } }]);
    const [, agents, workspaces] = refresh();
    if (agents.type !== 'agents' || workspaces.type !== 'workspaces') throw new Error('Expected refreshed inventory');
    expect(agents.agents.slice(0, atlas.agents.length)).toEqual(atlas.agents);
    const [first, second] = agents.agents.slice(atlas.agents.length);
    expect(first).toMatchObject({ pane_id: 'w4:demo-p1', tab_id: 'w4:demo-t1', tab_number: 3, agent: 'codex', cwd: command.cwd, updated_at: 1_786_525_501_000 });
    expect(second).toMatchObject({ pane_id: 'w5:demo-p2', tab_id: 'w5:demo-t2', tab_number: 2, agent: 'pi', updated_at: 1_786_525_502_000 });
    for (const key of ['pane_id', 'tab_id', 'terminal_id', 'agent_session_id'] as const) expect(first[key]).not.toBe(second[key]);
    expect(workspaces.workspaces.map(({ pane_count, tab_count }) => [pane_count, tab_count])).toEqual([[2, 2], [3, 3], [2, 2]]);
    const firstLog = readPane(relay, paneCommand(atlas.id, first)).content;
    expect(firstLog).toMatch(/^Codex · sample-run\n/);
    expect(firstLog).toContain('• Checked sample index');
    const reply = sampleReply('codex', 'Try the preview', 'fix/one');
    expect(firstLog.split('\n').slice(-reply.lines.length - 3)).toEqual([
      'Sample agent ready in /home/demo/project-fix-one. No process was started.',
      '', 'You: Try the preview', ...reply.lines,
    ]);
    expect(createDemoResponder(atlas.id).respond({ type: 'refresh_agents' })).toEqual(initial);
    expect(reader.respond({ type: 'refresh_agents' })).toContainEqual({ type: 'agents', agents: birch.agents });
  });

  it.each([
    ['codex', 'Codex', '• Checked sample index'],
    ['claude', 'Claude Code', '● Bash(make docs)'],
    ['pi', 'Pi', 'Edit docs/release-notes.md'],
    ['opencode', 'OpenCode', '# Bash: check the search fixtures'],
  ])('seeds an idle %s launch with a full sample transcript without a task', (profile_id, label, signature) => {
    const [atlas] = createDemoScene();
    const relay = createDemoResponder(atlas.id);
    const [result] = relay.respond({
      type: 'agent_start', request_id: 'idle-launch', profile_id, name: 'sample-run', cwd: '/home/demo/project',
    });
    expect(result).toMatchObject({ ok: true, data: { pane_id: 'w1:demo-p1' } });
    const [, inventory] = relay.respond({ type: 'refresh_agents' });
    if (inventory.type !== 'agents') throw new Error('Expected refreshed agents');
    const agent = inventory.agents.at(-1)!;
    expect(agent.status).toBe('idle');
    const rows = readPane(relay, paneCommand(atlas.id, agent)).content.split('\n');
    expect(rows[0]).toBe(`${label} · sample-run`);
    expect(rows).toContain(signature);
    expect(rows.filter((row) => row.trim()).length).toBeGreaterThanOrEqual(23);
    expect(rows.at(-1)).toBe('Sample agent ready in /home/demo/project. No process was started.');
  });

  it('reports the linked worktree branch in both git status replies and the inspector', () => {
    const [atlas] = createDemoScene();
    const relay = createDemoResponder(atlas.id);
    const fixture = paneCommand(atlas.id, atlas.agents.find(({ pane_id }) => pane_id === 'w4:p2')!);
    expect(relay.respond({ ...fixture, type: 'submit_prompt', text: 'git status' })).toMatchObject([{ ok: true }, { type: 'agent_update' }]);
    const { content } = readPane(relay, fixture);
    expect(content).toContain('On branch fix/one');
    expect(content).not.toContain('On branch main');
    expect(relay.respond({ ...fixture, type: 'workspace_git_status' })).toMatchObject([{ ok: true, data: { branch: 'fix/one' } }]);
  });

  it('serves workspace files and the matching unified diff only for exact Atlas targets', () => {
    const [atlas, birch] = createDemoScene();
    const relay = createDemoResponder(atlas.id);
    const target = paneCommand(atlas.id, atlas.agents[2]);
    const path = 'docs/release-notes.md';
    const code = 'src/search/Results.svelte';
    expect(relay.respond({ ...target, type: 'workspace_tree' })).toMatchObject([{
      ok: true, request_id: target.request_id, data: { root: '/home/demo/project', entries: [
        ...['docs', 'src', 'src/search'].map((path) => ({ path, kind: 'directory' })),
        ...['README.md', path, code].map((path) => ({ path, kind: 'file' })),
      ] },
    }]);
    expect(relay.respond({ ...target, type: 'workspace_git_status' })).toMatchObject([{ ok: true, data: {
      available: true, branch: 'main', files: [{ path, status: ' M' }, { path: code, status: ' M' }],
    } }]);
    for (const [file, mediaType, hunks, before, after] of [
      [path, 'text/markdown', 3, '-Roll out to everyone.', '+Start with the search preview group.'],
      [code, 'text/plain', 2, '-  <p>No results.</p>', '+    searchBox?.focus();'],
    ] as const) {
      const [preview] = relay.respond({ ...target, type: 'workspace_file', path: file });
      const [change] = relay.respond({ ...target, type: 'workspace_git_diff', path: file });
      expect(preview).toMatchObject({ ok: true, data: { path: file, kind: 'text', media_type: mediaType } });
      expect(change).toMatchObject({ ok: true, data: { path: file } });
      if (!('data' in preview) || !('data' in change)) throw new Error('Expected file and diff');
      const [header, ...sections] = String(change.data.diff).split(/\n(?=@@ )/);
      expect(header).toBe(`diff --git a/${file} b/${file}\n--- a/${file}\n+++ b/${file}`);
      expect(sections).toHaveLength(hunks);
      expect(change.data.diff).toContain(before);
      expect(change.data.diff).toContain(after);
      let previousEnd = 0;
      for (const section of sections) {
        const [heading, ...rows] = section.split('\n');
        const [, oldCount, newStart, newCount] = heading.match(/^@@ -\d+,(\d+) \+(\d+),(\d+) @@$/)!.map(Number);
        expect(rows.filter((row) => !row.startsWith('+'))).toHaveLength(oldCount);
        expect(newStart).toBeGreaterThan(previousEnd);
        const afterRows = rows.filter((row) => !row.startsWith('-')).map((row) => row.slice(1));
        expect(afterRows).toHaveLength(newCount);
        expect(String(preview.data.text).split('\n').slice(newStart - 1, newStart - 1 + newCount)).toEqual(afterRows);
        previousEnd = newStart + newCount;
      }
    }
    for (const type of ['workspace_tree', 'workspace_file', 'workspace_git_status', 'workspace_git_diff']) {
      expect(relay.respond({ ...target, type, path, target: { ...target.target, terminal_id: 'stale-terminal' } }))
        .toMatchObject([{ ok: false, phase: 'not_started' }]);
      expect(createDemoResponder(birch.id).respond({ ...paneCommand(birch.id, birch.agents[0]), type, path }))
        .toMatchObject([{ ok: false, phase: 'not_started' }]);
    }
    for (const invalid of ['../README.md', '/README.md', 'docs', 'toString']) {
      expect(relay.respond({ ...target, type: 'workspace_file', path: invalid })).toMatchObject([{ ok: false }]);
    }
    expect(relay.respond({ ...target, type: 'workspace_git_diff', path: 'README.md' })).toMatchObject([{ ok: false }]);
  });

  it('returns fictional devices and only the bitmap for Herdr demo', () => {
    const relay = createDemoResponder('demo-atlas');
    const reader = createDemoResponder('demo-birch');
    const devices = { type: 'device_list', request_id: 'devices' };
    expect(relay.respond(devices)).toMatchObject([{ request_id: 'devices', ok: true, data: { devices: [
      { name: 'Slate Tablet', role: 'controller', current: true, device_id: 'demo-atlas-device', credential_id: 'demo-atlas-credential' },
      { name: 'Field Phone', role: 'reader', current: false },
    ] } }]);
    expect(reader.respond(devices)).toMatchObject([{ ok: true, data: { devices: [
      { name: 'Pocket Reader', role: 'reader', current: true, device_id: 'demo-birch-device', credential_id: 'demo-birch-credential' },
    ] } }]);
    const qr = { type: 'qr_code', request_id: 'qr', text: 'Herdr demo' };
    const [reply] = relay.respond(qr);
    if (!('data' in reply)) throw new Error('Expected sample QR command result');
    // PackedQR("Herdr demo") from internal/setuphelper, without the quiet zone.
    expect(reply.data).toEqual({ size: 21, modules: '/lv8FZBukrt05duprsEpB/qv4AAAqkiSDIzL/f/osiNvHABB6/k+cEwjuuiF0pyupSsEOy/rLYA=' });
    expect(qrBitmap(reply.data.size, reply.data.modules)?.path).toMatch(/^M0 0h1v1h-1z/);
    expect(relay.respond({ ...qr, text: 'https://sample.invalid/#setup=not-a-link' }))
      .toMatchObject([{ request_id: 'qr', ok: false, phase: 'not_started' }]);
    expect(reader.respond(qr)).toMatchObject([{ ok: false, phase: 'not_started' }]);
  });

  it('refuses unknown computers instead of returning a default computer', () => {
    expect(() => createDemoResponder('unknown')).toThrow('Unknown demo computer.');
  });
});

describe('demo transport', () => {
  const config = { id: 'demo-atlas', label: 'Atlas', url: 'wss://demo-atlas.invalid', token: '' };
  beforeEach(() => sessionStorage.setItem(demoEpochKey, String(epoch)));

  it('reports connected before the handshake and delivers replies asynchronously', async () => {
    const received: unknown[] = [];
    const transport = createRelayTransport(config, {
      onStatus(status, detail) {
        received.push([status, detail]);
        expect(transport.send({ type: 'refresh_agents' })).toBe(true);
      },
      onMessage: (message) => received.push(message.type),
    });
    expect(transport.kind).toBe('websocket');
    expect(transport.send({ type: 'ping' })).toBe(false);
    transport.connect();
    transport.connect();
    expect(received).toEqual([]);
    expect(transport.send({ type: 'ping' })).toBe(false);
    await Promise.resolve();
    expect(received).toEqual([['connected', { path: 'websocket' }], 'push_config', 'speech_voices']);
    await Promise.resolve();
    expect(received.slice(3)).toEqual(['inventory_status', 'agents', 'workspaces']);
  });

  it('suppresses queued connection and command callbacks after closing', async () => {
    const received: string[] = [];
    const handlers = {
      onStatus: () => received.push('status'),
      onMessage: (message: { type?: string }) => received.push(message.type ?? ''),
    };
    const abandoned = createRelayTransport(config, handlers);
    abandoned.connect();
    abandoned.close();
    await Promise.resolve();
    expect(received).toEqual([]);

    const transport = createRelayTransport(config, handlers);
    transport.connect();
    await Promise.resolve();
    expect(transport.send({ type: 'ping' })).toBe(true);
    transport.close();
    transport.connect();
    expect(transport.send({ type: 'ping' })).toBe(false);
    await Promise.resolve();
    expect(received).toEqual(['status', 'push_config', 'speech_voices']);
    expect(() => createRelayTransport({ ...config, id: 'unknown' }, handlers)).toThrow('Unknown demo computer.');
  });
});
