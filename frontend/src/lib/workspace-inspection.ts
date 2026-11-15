import { CommandError } from './command-error';
import { relayStore } from './store';
import type { Agent, WorkspaceFile, WorkspaceGitDiff, WorkspaceGitStatus, WorkspaceTree } from './types';

function workspaceInspectionAvailable(agent: Agent): void {
  const connection = relayStore.connection(agent.relay_id);
  if (!connection?.capabilities.includes('workspace_inspection')) {
    throw new CommandError('This relay does not support workspace inspection.');
  }
  if (!String(agent.cwd || '').trim()) {
    throw new CommandError('This agent does not report a workspace path.');
  }
}

export async function loadWorkspaceTree(agent: Agent): Promise<WorkspaceTree> {
  workspaceInspectionAvailable(agent);
  const result = await relayStore.sendToAgent(agent, { type: 'workspace_tree' }, 20_000);
  const data = result.data as unknown as WorkspaceTree;
  if (!data || typeof data.root !== 'string' || !Array.isArray(data.entries)) {
    throw new CommandError('Relay returned an invalid workspace tree.');
  }
  return data;
}

export async function loadWorkspaceFile(agent: Agent, path: string): Promise<WorkspaceFile> {
  workspaceInspectionAvailable(agent);
  const result = await relayStore.sendToAgent(agent, { type: 'workspace_file', path }, 20_000);
  const data = result.data as unknown as WorkspaceFile;
  if (!data || data.path !== path || !['text', 'image'].includes(data.kind)) {
    throw new CommandError('Relay returned an invalid workspace preview.');
  }
  return data;
}

export async function loadWorkspaceGitStatus(agent: Agent): Promise<WorkspaceGitStatus> {
  workspaceInspectionAvailable(agent);
  const result = await relayStore.sendToAgent(agent, { type: 'workspace_git_status' }, 20_000);
  const data = result.data as unknown as WorkspaceGitStatus;
  if (!data || typeof data.available !== 'boolean' || !Array.isArray(data.files)) {
    throw new CommandError('Relay returned an invalid Git status.');
  }
  return data;
}

export async function loadWorkspaceGitDiff(agent: Agent, path: string): Promise<WorkspaceGitDiff> {
  workspaceInspectionAvailable(agent);
  const result = await relayStore.sendToAgent(agent, { type: 'workspace_git_diff', path }, 20_000);
  const data = result.data as unknown as WorkspaceGitDiff;
  if (!data || data.path !== path || typeof data.diff !== 'string') {
    throw new CommandError('Relay returned an invalid Git diff.');
  }
  return data;
}
