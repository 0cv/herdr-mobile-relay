import { agentNeedsInspection, agentNeedsResponse } from './agents';
import type { Agent } from './types';

export function promptDispatchRestriction(agent: Agent, screenPicker = false): string {
  return agentNeedsResponse(agent) || agentNeedsInspection(agent) || screenPicker
    ? 'Draft only — use structured controls or advanced input in Terminal.'
    : '';
}
