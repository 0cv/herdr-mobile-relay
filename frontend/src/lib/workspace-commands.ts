import { CommandError } from './command-error';
import { relayStore } from './store';
import type { CommandResult, RelayWorkspace, WorktreeListing } from './types';

function workspaceManagementAvailable(relayId: string, capability = 'workspace_management') {
  const connection = relayStore.connection(relayId);
  if (!connection?.capabilities.includes(capability)
    && !(['workspace_rename', 'workspace_reorder'].includes(capability) && connection?.capabilities.includes('workspace_management'))) {
    throw new CommandError(capability === 'worktree_management'
      ? 'This relay does not support worktree management'
      : 'This relay does not support workspace management');
  }
  return connection;
}

export async function createWorkspace(relayId: string, cwd: string, label: string): Promise<CommandResult> {
  workspaceManagementAvailable(relayId);
  const result = await relayStore.sendCommand(relayId, { type: 'workspace_create', cwd, label }, 45_000);
  relayStore.requestAgents();
  return result;
}

export async function renameWorkspace(workspace: RelayWorkspace, label: string): Promise<CommandResult> {
  workspaceManagementAvailable(workspace.relay_id, 'workspace_rename');
  const result = await relayStore.sendCommand(workspace.relay_id, {
    type: 'workspace_rename', workspace_id: workspace.workspace_id, label,
  });
  relayStore.requestAgents();
  return result;
}

export async function reorderWorkspaceBlock(
  relayId: string,
  workspaceIds: string[],
  beforeWorkspaceId: string,
  legacyInsertIndex: number,
): Promise<CommandResult> {
  const connection = workspaceManagementAvailable(relayId, 'workspace_reorder');
  if (!workspaceIds.length || new Set(workspaceIds).size !== workspaceIds.length) {
    throw new CommandError('Workspace selection is invalid');
  }
  const payload = connection.capabilities.includes('workspace_reorder_block')
    ? {
        type: 'workspace_reorder',
        workspace_ids: workspaceIds,
        before_workspace_id: beforeWorkspaceId,
      }
    : workspaceIds.length === 1 && Number.isInteger(legacyInsertIndex) && legacyInsertIndex >= 0
      ? {
          type: 'workspace_reorder',
          workspace_id: workspaceIds[0],
          insert_index: legacyInsertIndex,
        }
      : null;
  if (!payload) {
    throw new CommandError('Update Herdr to reorder a workspace with linked worktrees');
  }
  const result = await relayStore.sendCommand(relayId, payload);
  relayStore.requestAgents();
  return result;
}

export async function closeWorkspace(
  workspace: RelayWorkspace,
  options: { closeGroup?: boolean; expectedWorkspaceIds?: string[] } = {},
): Promise<CommandResult> {
  workspaceManagementAvailable(workspace.relay_id);
  const payload: Record<string, unknown> = {
    type: 'workspace_close',
    workspace_id: workspace.workspace_id,
  };
  if (options.closeGroup === true) {
    payload.close_group = true;
    payload.expected_workspace_ids = options.expectedWorkspaceIds || [];
  }
  const result = await relayStore.sendCommand(workspace.relay_id, payload, 30_000);
  relayStore.requestAgents();
  return result;
}

export async function listWorktrees(workspace: RelayWorkspace): Promise<WorktreeListing> {
  workspaceManagementAvailable(workspace.relay_id, 'worktree_management');
  const result = await relayStore.sendCommand(workspace.relay_id, {
    type: 'worktree_list', workspace_id: workspace.workspace_id,
  }, 30_000);
  const listing = result.data as unknown as WorktreeListing;
  if (!listing?.source || !Array.isArray(listing.worktrees)) {
    throw new CommandError('Relay returned an invalid worktree listing');
  }
  return listing;
}

export async function createWorktree(
  workspace: RelayWorkspace,
  options: { branch: string; base?: string; label?: string },
): Promise<CommandResult> {
  workspaceManagementAvailable(workspace.relay_id, 'worktree_management');
  const result = await relayStore.sendCommand(workspace.relay_id, {
    type: 'worktree_create',
    workspace_id: workspace.workspace_id,
    branch: options.branch,
    base: options.base || '',
    label: options.label || '',
  }, 75_000);
  relayStore.requestAgents();
  return result;
}

export async function openWorktree(
  workspace: RelayWorkspace,
  options: { path?: string; branch?: string; label?: string },
): Promise<CommandResult> {
  workspaceManagementAvailable(workspace.relay_id, 'worktree_management');
  const result = await relayStore.sendCommand(workspace.relay_id, {
    type: 'worktree_open',
    workspace_id: workspace.workspace_id,
    path: options.path || '',
    branch: options.branch || '',
    label: options.label || '',
  }, 75_000);
  relayStore.requestAgents();
  return result;
}

export async function removeWorktree(workspace: RelayWorkspace, force = false): Promise<CommandResult> {
  workspaceManagementAvailable(workspace.relay_id, 'worktree_management');
  const result = await relayStore.sendCommand(workspace.relay_id, {
    type: 'worktree_remove', workspace_id: workspace.workspace_id, force,
  }, 75_000);
  relayStore.requestAgents();
  return result;
}
