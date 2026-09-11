import { PhaseBudget } from './budget';
import { AppiumClient, isFatalDriverError } from './webdriver';

export const CONFIRMATION_SETTINGS_COMMAND_MS = 2_000;
export const CONFIRMATION_RESTORE_MS = 2 * CONFIRMATION_SETTINGS_COMMAND_MS;
export const IOS_CONFIRMATION_SETTINGS = { waitForIdleTimeout: 1, animationCoolOffTimeout: 0.2 };

function touchedSettings(value: unknown): typeof IOS_CONFIRMATION_SETTINGS {
  if (!value || typeof value !== 'object') throw new Error('IOS_CONFIRMATION_SETTINGS: malformed settings');
  const result = {} as typeof IOS_CONFIRMATION_SETTINGS;
  for (const key of Object.keys(IOS_CONFIRMATION_SETTINGS) as Array<keyof typeof IOS_CONFIRMATION_SETTINGS>) {
    const setting = (value as Record<string, unknown>)[key];
    if (typeof setting !== 'number' || !Number.isFinite(setting) || setting < 0) {
      throw new Error(`IOS_CONFIRMATION_SETTINGS: unsupported or malformed ${key}`);
    }
    result[key] = setting;
  }
  return result;
}

function assertSettings(actual: unknown, expected: typeof IOS_CONFIRMATION_SETTINGS): void {
  const observed = touchedSettings(actual);
  if (Object.keys(expected).some(key => observed[key as keyof typeof observed] !== expected[key as keyof typeof expected])) {
    throw new Error('IOS_CONFIRMATION_SETTINGS: settings readback mismatch');
  }
}

export async function withIOSConfirmationSettings<T>(
  driver: AppiumClient,
  parent: PhaseBudget,
  operationMs: number,
  operation: (phase: PhaseBudget) => Promise<T>,
): Promise<T> {
  const overhead = 3 * CONFIRMATION_SETTINGS_COMMAND_MS + CONFIRMATION_RESTORE_MS;
  if (parent.remainingMs < operationMs + overhead) throw new Error('IOS_CONFIRMATION_SETTINGS: insufficient whole transaction allowance');
  const saved = touchedSettings(await driver.settings(CONFIRMATION_SETTINGS_COMMAND_MS));
  let attempted = false;
  let failed = false;
  let originalError: unknown;
  let result: T | undefined;
  try {
    if (parent.remainingMs < operationMs + 2 * CONFIRMATION_SETTINGS_COMMAND_MS + CONFIRMATION_RESTORE_MS) throw new Error('IOS_CONFIRMATION_SETTINGS: insufficient apply and operation allowance');
    attempted = true;
    await driver.updateSettings(IOS_CONFIRMATION_SETTINGS, CONFIRMATION_SETTINGS_COMMAND_MS);
    if (parent.remainingMs < operationMs + CONFIRMATION_SETTINGS_COMMAND_MS + CONFIRMATION_RESTORE_MS) throw new Error('IOS_CONFIRMATION_SETTINGS: insufficient readback and operation allowance');
    assertSettings(await driver.settings(CONFIRMATION_SETTINGS_COMMAND_MS), IOS_CONFIRMATION_SETTINGS);
    if (parent.remainingMs < operationMs + CONFIRMATION_RESTORE_MS) throw new Error('IOS_CONFIRMATION_SETTINGS: insufficient operation and restoration allowance');
    result = await operation(parent.phaseView('ios-confirmation-operation', parent.remainingMs, CONFIRMATION_RESTORE_MS));
  } catch (error) {
    failed = true;
    originalError = error;
  }
  if (attempted && !isFatalDriverError(originalError) && !driver.snapshot().unusable && !driver.snapshot().firstFatal) {
    try {
      if (parent.remainingMs < CONFIRMATION_RESTORE_MS) throw new Error('IOS_CONFIRMATION_SETTINGS: insufficient restoration allowance');
      await driver.updateSettings(saved, CONFIRMATION_SETTINGS_COMMAND_MS);
      if (parent.remainingMs < CONFIRMATION_SETTINGS_COMMAND_MS) throw new Error('IOS_CONFIRMATION_SETTINGS: insufficient restoration readback allowance');
      assertSettings(await driver.settings(CONFIRMATION_SETTINGS_COMMAND_MS), saved);
    } catch (error) {
      if (!failed) throw error;
    }
  }
  if (failed) throw originalError;
  return result as T;
}
