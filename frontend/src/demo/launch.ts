export { validAgentName, launchNamePart, suggestedLaunchName } from '../lib/launch';

export const launchAbandoned = 'Sample launch confirmation stopped. Check the Agents list.';
export const launchConfirmed = 'Sample agent added. No process was started.';
export const launchError = (error: Error) => error.message;
export const launchWorkspaceSuffix = '';
export const launchDirectoryUnavailable = 'Sample folders are unavailable.';
export const launchTaskHint = 'Adds an agent to this visit’s sample data. The initial task gets a scripted reply; no process starts on a computer.';
