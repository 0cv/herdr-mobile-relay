import { isQualificationFatal } from '../support/oracle';
import type { PhaseBudget } from '../support/budget';
import {
  ariaLabel,
  ariaLabelPrefix,
  buttonText,
  css,
  isFatalDriverError as isFatalAppiumError,
  isRetryableElementLookupError,
  minimumDriverRequestMs,
  textLocator,
  WebDriverError,
  type AppiumClient,
} from '../support/webdriver';

export interface WebContext {
  readonly driver: AppiumClient;
  readonly budget: PhaseBudget;
  attachToInstalledView(timeoutMs?: number): Promise<void>;
}

export type WebClick = (element: string, deadline: number, clickTimeout: number) => Promise<void>;

export const preferenceValueScript = "return localStorage.getItem('herdr_home_workspace_layout') || ''";
export const promptDraftScript = "return document.querySelector('textarea[aria-label=\\\"Prompt\\\"]')?.value || ''";

export function isFatalDriverError(error: unknown): boolean {
  return isFatalAppiumError(error) || isQualificationFatal(error);
}

export function isExpectedOrigin(value: string, origin: string): boolean {
  try {
    return new URL(value).origin === new URL(origin).origin;
  } catch {
    return false;
  }
}

export async function setPreference(clickWebText: (text: string) => Promise<void>, preference: string): Promise<void> {
  await clickWebText('Settings');
  await clickWebText(preference === 'state' ? 'By State' : 'Mixed');
}

async function webControlReady(context: WebContext, element: string, deadline: number): Promise<boolean> {
  const { driver } = context;
  const disabledTimeout = deadline - context.budget.now();
  if (disabledTimeout < minimumDriverRequestMs) return false;
  const disabled = await driver.attribute(element, 'disabled', disabledTimeout);
  if (disabled !== null && disabled !== 'false') return false;
  const ariaDisabledTimeout = deadline - context.budget.now();
  if (ariaDisabledTimeout < minimumDriverRequestMs) return false;
  const ariaDisabled = await driver.attribute(element, 'aria-disabled', ariaDisabledTimeout);
  if (ariaDisabled === 'true') return false;
  const hiddenTimeout = deadline - context.budget.now();
  if (hiddenTimeout < minimumDriverRequestMs) return false;
  const hidden = await driver.attribute(element, 'hidden', hiddenTimeout);
  if (hidden !== null && hidden !== 'false') return false;
  const ariaHiddenTimeout = deadline - context.budget.now();
  if (ariaHiddenTimeout < minimumDriverRequestMs) return false;
  const ariaHidden = await driver.attribute(element, 'aria-hidden', ariaHiddenTimeout);
  if (ariaHidden === 'true') return false;
  const rectTimeout = deadline - context.budget.now();
  if (rectTimeout < minimumDriverRequestMs) return false;
  const rect = await driver.elementRect(element, rectTimeout);
  return rect.width > 0 && rect.height > 0;
}

function defaultClick(context: WebContext): WebClick {
  return async (element, _deadline, clickTimeout) => {
    await context.driver.click(element, clickTimeout);
  };
}

export async function clickWebText(context: WebContext, text: string, click: WebClick = defaultClick(context)): Promise<void> {
  const { driver } = context;
  const deadline = context.budget.now() + Math.min(30_000, context.budget.remainingMs);
  let lastError = '';
  while (context.budget.now() < deadline) {
    const remaining = deadline - context.budget.now();
    if (remaining < minimumDriverRequestMs) break;
    try {
      await context.attachToInstalledView(remaining);
      const findTimeout = deadline - context.budget.now();
      if (findTimeout < minimumDriverRequestMs) break;
      const locators = [buttonText(text), ariaLabel(text), ariaLabelPrefix(text), textLocator(text)];
      for (const locator of locators) {
        const locatorTimeout = deadline - context.budget.now();
        if (locatorTimeout < minimumDriverRequestMs) break;
        try {
          const element = await driver.findAnyOnce([locator], locatorTimeout);
          const controlTimeout = deadline - context.budget.now();
          if (controlTimeout < minimumDriverRequestMs) break;
          if (await webControlReady(context, element, deadline)) {
            const clickTimeout = deadline - context.budget.now();
            if (clickTimeout < minimumDriverRequestMs) break;
            await click(element, deadline, clickTimeout);
            return;
          }
          lastError = `${text} is disabled, hidden, or empty`;
        } catch (error) {
          if (isFatalDriverError(error)) throw error;
          if (error instanceof WebDriverError && !isRetryableElementLookupError(error)) throw error;
          lastError = error instanceof Error ? error.message : String(error);
        }
      }
    } catch (error) {
      if (isFatalDriverError(error)) throw error;
      if (error instanceof WebDriverError && !isRetryableElementLookupError(error)) throw error;
      lastError = error instanceof Error ? error.message : String(error);
    }
    const waitMs = Math.min(250, Math.max(0, deadline - context.budget.now() - minimumDriverRequestMs));
    if (waitMs > 0) await context.budget.sleep(waitMs);
  }
  throw new Error(`APPIUM_BUTTON: ${text}: ${lastError || 'control was not usable before the deadline'}`);
}

export async function clickDialogText(context: WebContext, dialogId: string, text: string, click: WebClick = defaultClick(context)): Promise<void> {
  const { driver } = context;
  const deadline = context.budget.now() + Math.min(30_000, context.budget.remainingMs);
  let lastError = '';
  while (context.budget.now() < deadline) {
    const remaining = deadline - context.budget.now();
    if (remaining < minimumDriverRequestMs) break;
    try {
      await context.attachToInstalledView(remaining);
      const findTimeout = deadline - context.budget.now();
      if (findTimeout < minimumDriverRequestMs) break;
      const buttons = await driver.findAll(css(`#${dialogId} button`), findTimeout);
      for (const button of buttons) {
        const textTimeout = deadline - context.budget.now();
        if (textTimeout < minimumDriverRequestMs) break;
        if ((await driver.text(button, textTimeout)).trim() === text) {
          const clickTimeout = deadline - context.budget.now();
          if (clickTimeout < minimumDriverRequestMs) break;
          await click(button, deadline, clickTimeout);
          return;
        }
      }
      lastError = `${text} is not visible in ${dialogId}`;
    } catch (error) {
      if (isFatalDriverError(error)) throw error;
      lastError = error instanceof Error ? error.message : String(error);
    }
    const waitMs = Math.min(250, Math.max(0, deadline - context.budget.now() - minimumDriverRequestMs));
    if (waitMs > 0) await context.budget.sleep(waitMs);
  }
  throw new Error(`APPIUM_DIALOG_BUTTON: ${dialogId}/${text}: ${lastError}`);
}
