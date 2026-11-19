const noop = () => {};

export const observeAgentAttention = noop;
export const initializePush = noop;
export const notificationsSupported = () => false;
export const pushSupported = notificationsSupported;
export const notificationsEnabled = notificationsSupported;
export const pushOptedIn = notificationsSupported;
export const createPushOpenRouter = () => ({ route: noop, stop: noop });
export const createViewedPaneReporter = () => noop;
export const settingsUpdateLabel = () => 'Settings';
export const createRelayUpdateObserver = () => noop;
export const mountTerminalWakeLock = () => noop;
export const detectTerminalMenu = () => null;
export const terminalTextInputMode = () => null;
