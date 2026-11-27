import { agentStatusGroup } from '../lib/agents';
import { normalizeFrontendTargetRef } from '../lib/resource-id';
import { stripAnsi } from '../lib/terminal';
import { sampleChecks } from './checks';
import { sampleReply } from './replies';
import { sampleDevices, sampleGitDiff, sampleGitStatus, sampleQr, sampleSpeechVoices, sampleWorkspaceFile, sampleWorkspaceTree } from './samples';
import { createDemoConversation, createDemoScene, demoProfiles, type DemoAgent } from './scene';
import { launchTranscript, transcripts } from './transcripts';

const paneActions = new Set([
  'read_pane', 'acknowledge_pane', 'get_conversation_history', 'submit_prompt', 'send_terminal_text', 'send_keys',
  'respond', 'answer_question', 'workspace_tree', 'workspace_file', 'workspace_git_status', 'workspace_git_diff',
]);
const computerActions = new Set(['list_directories', 'agent_start', 'device_list', 'qr_code']);
type TerminalInput = { text: string; check: number | null };

function inputText(value: unknown): string {
  return typeof value === 'string' ? stripAnsi(value).replace(/\r\n?/g, '\n').replace(/[\x00-\x08\x0b-\x1f\x7f-\x9f]/g, '') : '';
}

export function createDemoResponder(computerId: string, epoch: number) {
  const computer = createDemoScene(epoch).find(({ id }) => id === computerId);
  if (!computer) throw new Error('Unknown demo computer.');
  const screen = new Map(computer.agents.map(({ terminal_id }) => [terminal_id, transcripts[terminal_id]]));
  const inputs = new Map<string, TerminalInput>();
  const controller = computer.role === 'controller';
  const history = controller ? createDemoConversation(epoch) : [];
  const folders = computer.workspaces.flatMap(({ cwd }) => cwd ? [{ path: cwd, name: cwd.split('/').at(-1)! }] : []);
  let mutation = 0;
  let launches = 0;
  const nextTime = () => epoch + ++mutation * 1_000;
  const agentUpdate = (agent: DemoAgent) => ({ ...structuredClone(agent), type: 'agent_update' as const });

  function terminalInput(agent: DemoAgent): TerminalInput {
    const input = inputs.get(agent.terminal_id) ?? { text: '', check: null };
    inputs.set(agent.terminal_id, input);
    return input;
  }

  function append(agent: DemoAgent, ...lines: string[]): void {
    screen.set(agent.terminal_id, [screen.get(agent.terminal_id), ...lines].join('\n'));
  }

  function recordActivity(agent: DemoAgent): number {
    agent.pane_revision += 1;
    agent.activity_seq += 1;
    return agent.updated_at = agent.last_active_at = nextTime();
  }

  const sampleBranch = (agent: DemoAgent): string => {
    const workspace = computer.workspaces.find(({ workspace_id }) => workspace_id === agent.workspace_id);
    return workspace?.worktree?.is_linked_worktree ? workspace.label : 'main';
  };

  function runPrompt(agent: DemoAgent, text: string, echo = true): void {
    const reply = sampleReply(agent.agent, text, sampleBranch(agent));
    append(agent, ...(echo ? ['', `You: ${text}`] : []), ...reply.lines);
    const timestamp = new Date(recordActivity(agent)).toISOString();
    if (!agent.conversation_history_available) return;
    const user = `notes-${history.length + 1}`;
    const assistant = `notes-${history.length + 2}`;
    history.push(
      { id: user, role: 'user', text, timestamp },
      { id: assistant, role: 'assistant', text: reply.text, timestamp,
        tools: reply.tools.map((tool, index) => ({ ...tool, id: `${assistant}-tool-${index + 1}` })) },
    );
  }

  function applyKey(agent: DemoAgent, key: string): string {
    const input = terminalInput(agent);
    const show = (line: string) => {
      append(agent, line);
      return line;
    };
    switch (key) {
      case 'escape':
        input.text = '';
        input.check = null;
        return show('Sample input and check selection cleared.');
      case 'ctrl+c':
        input.text = '';
        input.check = null;
        return show('^C · Sample step interrupted.');
      case 'up':
      case 'left':
      case 'down':
      case 'right': {
        if (agent.status === 'blocked') return show('Sample checks are unavailable while this agent is waiting for a choice.');
        const forward = key === 'down' || key === 'right';
        const start = input.check ?? (forward ? -1 : 0);
        input.check = (start + (forward ? 1 : -1) + sampleChecks.length) % sampleChecks.length;
        return show(`› Sample check ${input.check + 1}/${sampleChecks.length}: ${sampleChecks[input.check].command}`);
      }
      case 'enter': {
        const { text, check } = input;
        input.text = '';
        input.check = null;
        if (agent.status === 'blocked') {
          return show(text ? 'Sample text committed. The waiting choice is unchanged.' : 'Use the waiting controls to make a choice.');
        }
        if (text.trim()) {
          runPrompt(agent, text, false);
          return 'Sample text committed. Scripted reply added to the log.';
        }
        if (check === null) return show('No sample input. Use the arrows to select a check.');
        const { command, result } = sampleChecks[check];
        append(agent, `Scripted check · $ ${command}`, result, 'No command ran.');
        recordActivity(agent);
        return `Scripted check · ${command} — ${result.split('\n').at(-1)}`;
      }
      default:
        append(agent, 'Demo response: Key input received; no terminal action ran.');
        return '';
    }
  }

  function finishInteraction(agent: DemoAgent, outcome: string): void {
    inputs.delete(agent.terminal_id);
    delete agent.attention_kind;
    delete agent.event_id;
    delete agent.prompt;
    delete agent.command;
    delete agent.approval_fingerprint;
    delete agent.options;
    delete agent.interaction;
    agent.status = 'working';
    recordActivity(agent);
    append(agent, '', outcome);
  }

  function inventoryStatus() {
    return {
      state: 'ready' as const,
      error_code: '',
      message: '',
      last_attempt_at: Math.floor(epoch / 1_000),
      last_success_at: Math.floor(epoch / 1_000),
      stale: false,
    };
  }

  return {
    initialMessages() {
      const handshake = {
        type: 'push_config' as const, protocol: 3, host: computer.label, home: computer.home,
        capabilities: [...computer.capabilities], inventory: inventoryStatus(),
        ...(controller ? { agent_profiles: structuredClone([...demoProfiles]) } : {}),
      };
      return controller ? [handshake, sampleSpeechVoices()] as const : [handshake] as const;
    },
    respond(message: Record<string, unknown>) {
      const { type, request_id } = message;
      if (type === 'ping') return [{ type: 'pong' as const }];
      if (type === 'refresh_agents') return [
        { type: 'inventory_status' as const, ...inventoryStatus() },
        { type: 'agents' as const, agents: structuredClone(computer.agents) },
        { type: 'workspaces' as const, workspaces: structuredClone(computer.workspaces) },
      ];
      if (type === 'get_activity') return [{ type: 'activity_history' as const, activities: [] }];
      if (typeof type !== 'string' || (!paneActions.has(type) && !computerActions.has(type))) return [{
        type: 'error' as const,
        error: { code: 'unknown_action' },
        detail: 'This action is not available in the demo.',
        ...(typeof request_id === 'string' && request_id ? { request_id } : {}),
      }];

      const result = { type: 'command_result' as const, action: type, request_id };
      const fail = (error: string) => [{ ...result, ok: false, phase: 'not_started', error }];
      const complete = (data: Record<string, unknown> = {}, phase = 'completed') => ({ ...result, ok: true, phase, data });
      if (typeof request_id !== 'string' || !request_id) return fail('Missing sample request ID.');
      if (type === 'device_list') return [complete({ devices: sampleDevices(computer, epoch) })];
      if (computerActions.has(type)) {
        if (!controller) return fail('This sample computer is read-only.');
        if (type === 'qr_code') {
          return message.text === 'Herdr demo' ? [complete({ ...sampleQr })] : fail('Only the sample demo QR is available.');
        }
        if (type === 'list_directories') {
          const path = message.path === '' ? computer.home : message.path;
          const home = path === computer.home;
          const folder = folders.find((entry) => entry.path === path);
          if (!home && !folder) return fail('That sample folder is unavailable.');
          return [complete({
            current: { path, label: folder?.name ?? 'Home' }, parent: home ? '' : computer.home,
            directories: home ? structuredClone(folders) : [],
          })];
        }
        const { cwd, workspace_id = '', profile_id, name, prompt = '' } = message;
        const profile = demoProfiles.find(({ id }) => id === profile_id);
        const workspace = computer.workspaces.find((entry) => workspace_id === ''
          ? entry.cwd === cwd : entry.workspace_id === workspace_id);
        if (!profile || !workspace || !folders.some(({ path }) => path === cwd)
          || typeof name !== 'string' || !/^[a-z][a-z0-9_-]{0,31}$/.test(name) || typeof prompt !== 'string') {
          return fail('Choose a sample profile, workspace and project folder, and enter a valid name.');
        }
        const id = ++launches;
        const workspaceId = workspace.workspace_id;
        const agent: DemoAgent = {
          server_session_id: computer.agents[0].server_session_id, pane_id: `${workspaceId}:demo-p${id}`,
          terminal_id: `atlas-launch-${id}-terminal`, agent_session_id: `atlas-launch-${id}-session`, generation: 1,
          tab_id: `${workspaceId}:demo-t${id}`, tab_label: name, tab_number: ++workspace.tab_count, tab_order: workspace.tab_count,
          workspace_id: workspaceId, cwd: String(cwd), agent: profile.id, project: name, status: 'idle',
          pane_revision: 1, activity_seq: 1,
        };
        workspace.pane_count += 1;
        computer.agents.push(agent);
        screen.set(agent.terminal_id, launchTranscript(profile, name, String(cwd)));
        const text = inputText(prompt);
        if (text.trim()) runPrompt(agent, text);
        else agent.updated_at = agent.last_active_at = nextTime();
        return [complete({ pane_id: agent.pane_id })];
      }
      const target = normalizeFrontendTargetRef(message.target);
      const agent = target && target.relay_id === computer.id
        && target.pane_id === message.pane_id && target.server_session_id === message.server_session_id
        && computer.agents.find((row) => row.pane_id === target.pane_id
          && row.server_session_id === target.server_session_id && row.terminal_id === target.terminal_id
          && row.generation === target.generation && row.agent_session_id === target.agent_session_id);
      if (!agent || !target) return fail('This sample agent is no longer available.');

      if (type === 'read_pane') {
        const lines = message.lines;
        if (typeof lines !== 'number' || !Number.isSafeInteger(lines) || lines < 1) return fail('Invalid terminal line budget.');
        return [{
          type: 'pane_content' as const, request_id, pane_id: agent.pane_id, target, format: 'plain',
          content: screen.get(agent.terminal_id)!.split('\n').slice(-lines).join('\n'), no_echo: false,
        }];
      }
      if (type === 'get_conversation_history') {
        const { cursor, limit, observe } = message;
        if (!agent.conversation_history_available) return fail('This sample agent has no conversation history.');
        if (cursor !== '' || observe !== false || typeof limit !== 'number' || !Number.isSafeInteger(limit) || limit < 1) {
          return fail('Invalid sample conversation request.');
        }
        return [complete({
          available: true, state: 'ready', mode: 'recent', source_revision: 'demo-notes',
          next_cursor: '', has_more: false, total: history.length, entries: structuredClone(history.slice(-limit)),
        })];
      }
      if (!controller) return fail('This sample computer is read-only.');

      switch (type) {
        case 'workspace_tree': return [complete(sampleWorkspaceTree(String(agent.cwd)))];
        case 'workspace_file': {
          const file = sampleWorkspaceFile(message.path);
          return file ? [complete(file)] : fail('That sample file is unavailable.');
        }
        case 'workspace_git_status': return [complete(sampleGitStatus(sampleBranch(agent)))];
        case 'workspace_git_diff': {
          const change = sampleGitDiff(message.path);
          return change ? [complete(change)] : fail('That sample change is unavailable.');
        }
        case 'acknowledge_pane': {
          if (agentStatusGroup(agent) !== 'done') return [complete()];
          agent.status = 'idle';
          agent.pane_revision += 1;
          return [complete(), agentUpdate(agent)];
        }
        case 'submit_prompt': {
          const text = inputText(message.text);
          if (!text.trim() || agent.status === 'blocked') return fail('Use the waiting controls or enter a sample prompt.');
          runPrompt(agent, text);
          return [complete(), agentUpdate(agent)];
        }
        case 'send_terminal_text': {
          const text = inputText(message.text);
          if (!text) return fail('Enter some sample terminal text.');
          terminalInput(agent).text += text;
          append(agent, '', `Typed: ${text}`, 'Demo: Text received, waiting for Enter.');
          return [complete()];
        }
        case 'send_keys': {
          const keys = message.keys;
          if (!Array.isArray(keys) || !keys.length || keys.some((key) => !key || inputText(key) !== key || key.includes('\n'))) {
            return fail('Invalid sample terminal keys.');
          }
          append(agent, '', `Keys: ${keys.join(', ')}`);
          const revision = agent.pane_revision;
          const feedback = keys.map((key) => applyKey(agent, key.toLowerCase())).filter(Boolean);
          const delivery = complete(feedback.length ? { message: feedback.join(' ') } : {});
          return agent.pane_revision !== revision ? [delivery, agentUpdate(agent)] : [delivery];
        }
        case 'respond': {
          const { index, total, choice, source, event_id, approval_fingerprint } = message;
          if (agent.status !== 'blocked' || agent.attention_kind !== 'approval' || !agent.options
            || event_id !== agent.event_id || approval_fingerprint !== agent.approval_fingerprint
            || source !== 'App' || total !== agent.options.length || typeof index !== 'number' || !Number.isInteger(index)
            || typeof choice !== 'string' || choice !== agent.options[index]) return fail('That sample approval choice is no longer available.');
          finishInteraction(agent, `Approval: ${choice}. Demo only; make test was not run.`);
          return [agentUpdate(agent), complete({}, 'confirmed')];
        }
        case 'answer_question': {
          const { interaction_id, selected_indices, other_selected, other_text, source } = message;
          const choice = Array.isArray(selected_indices) && selected_indices.length === 1
            && agent.interaction?.options.find(({ index }) => index === selected_indices[0]);
          if (agent.status !== 'blocked' || agent.attention_kind !== 'question' || interaction_id !== agent.interaction?.id
            || source !== 'App' || other_selected !== false || other_text !== '' || !choice) {
            return fail('That sample rollout choice is no longer available.');
          }
          finishInteraction(agent, `Rollout choice: ${choice.label}. Demo only; nothing was deployed.`);
          return [complete({}, 'confirmed')];
        }
        default: return fail('This action is not available in the demo.');
      }
    },
  };
}
