export type ActionPolicy = { controller: boolean; target: boolean; workspace: boolean };
const reads = [
  'cancel_speech', 'check_update', 'device_list', 'get_activity', 'get_conversation_history',
  'list_directories', 'list_slash_commands', 'pane_applied', 'push_open_ref', 'push_policy_get',
  'qr_code', 'read_pane', 'speech_voices_list', 'speak_text', 'unwatch_pane', 'watch_pane',
  'workspace_file', 'workspace_git_diff', 'workspace_git_status', 'workspace_tree', 'worktree_list',
];
const writes = [
  'acknowledge_pane', 'agent_clear', 'agent_rename', 'agent_restart', 'agent_start', 'agent_stop',
  'answer_question', 'clarify_question', 'clear_activities', 'copy_agent_response', 'deploy_app_update',
  'create_device_invitation', 'install_update', 'lease_pane_size', 'navigate_question',
  'register_app_origin', 'release_pane_size', 'rename_device', 'respond', 'send_keys', 'send_input',
  'send_filter_text', 'send_secret', 'reset_devices', 'revoke_device', 'send_text',
  'speech_voice_install', 'speech_voice_remove', 'submit_prompt', 'tab_reorder',
  'upload_begin', 'upload_chunk', 'upload_finish', 'upload_cancel', 'workspace_close',
  'workspace_create', 'workspace_rename', 'workspace_reorder', 'workspace_reorder_block',
  'worktree_create', 'worktree_open', 'worktree_remove',
];
// Devices may change their own notification configuration as readers; the relay
// still enforces ownership and the selected role on the authenticated request.
const ownDevice = [
  'push_policy_set', 'push_snooze', 'push_subscribe', 'push_test_device', 'push_unsubscribe', 'push_viewed_pane',
];
const targets = new Set([
  'acknowledge_pane', 'agent_clear', 'agent_rename', 'agent_restart', 'agent_stop', 'answer_question',
  'cancel_speech', 'clarify_question', 'copy_agent_response', 'get_conversation_history', 'lease_pane_size',
  'list_slash_commands', 'navigate_question', 'pane_applied', 'read_pane', 'release_pane_size', 'respond',
  'send_keys', 'send_input', 'send_filter_text', 'send_secret', 'send_text', 'speak_text',
  'submit_prompt', 'tab_reorder', 'unwatch_pane', 'watch_pane', 'upload_begin',
]);
const policies = new Map<string, ActionPolicy>();
for (const action of [...reads, ...writes, ...ownDevice]) policies.set(action, {
  controller: writes.includes(action), target: targets.has(action),
  workspace: action.startsWith('workspace_') || action.startsWith('worktree_') || action === 'agent_start',
});

export function actionPolicy(type: unknown): ActionPolicy | null {
  return typeof type === 'string' ? policies.get(type) ?? null : null;
}

/** Recovery never grants an operational target or dispatches a queued write. */
export const RECOVERY_ACTIONS = new Set(['refresh_agents', 'webrtc_offer', 'webrtc_ice', 'webrtc_close']);
export const CLEANUP_FOR = new Map([
  ['unwatch_pane', 'watch_pane'], ['release_pane_size', 'lease_pane_size'], ['upload_cancel', 'upload'],
]);
