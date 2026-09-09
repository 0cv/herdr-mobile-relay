import { mkdir, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { assertStandalone, type RuntimeIdentity } from '../support/oracle';
import { DiagnosticRecorder, writeBoundedText, writeSanitizedJson } from '../support/diagnostics';
import { PhaseBudget } from '../support/budget';
import { command, commandOutput } from '../support/process';
import { requireOwnedDevice } from '../support/device';
import {
  accessibility,
  accessibilityPrefix,
  AppiumClient,
  buttonText,
  css,
  delay,
  textLocator,
  type Locator,
} from '../support/webdriver';
import { runtimeScript, updateCompletionScript, type MobilePlatform, type PlatformOptions, type UpdateCompletionEvidence } from './types';

function iosLabelContains(value: string): Locator {
  return {
    using: 'xpath',
    value: `//*[contains(@label, '${value}') or contains(@name, '${value}') or contains(@value, '${value}') or contains(@text, '${value}')]`,
  };
}

export class IOSPlatform implements MobilePlatform {
  readonly name = 'ios' as const;
  readonly driver: AppiumClient;
  private readonly udid: string;
  private readonly origin: string;
  private readonly outputDir: string;
  private readonly budget: PhaseBudget;
  private readonly diagnostics: DiagnosticRecorder;
  private installedBundleId = '';
  private selectedInstalledContext = '';
  private lastIdentity?: RuntimeIdentity;
  private lastUrl = '';
  private keyboardDraft = '';
  private lastCompletion?: UpdateCompletionEvidence;
  private lastNativeActivity = '';
  private lastNativePid = '';

  constructor(private readonly options: PlatformOptions) {
    this.udid = options.deviceId || process.env.IOS_SIMULATOR_UDID || '';
    this.origin = options.origin.replace(/\/$/, '');
    this.outputDir = options.outputDir;
    this.budget = options.budget || new PhaseBudget('ios-run', { timeoutMs: 30 * 60_000, recoveryLimit: 4 });
    this.diagnostics = options.diagnostics || new DiagnosticRecorder();
    this.driver = new AppiumClient(options.appiumUrl);
    this.driver.setBudget(this.budget);
  }

  async startFreshDevice(): Promise<void> {
    if (!this.udid) throw new Error('IOS_TARGET: IOS_SIMULATOR_UDID is required');
    await requireOwnedDevice('ios', this.udid);
    const available = await commandOutput('xcrun', ['simctl', 'list', 'devices', 'available']);
    if (!available.includes(this.udid)) throw new Error(`IOS_TARGET: simulator ${this.udid} is not an available simulator`);
    await command('xcrun', ['simctl', 'boot', this.udid]).catch(() => undefined);
    await command('xcrun', ['simctl', 'bootstatus', this.udid, '-b'], 300_000);
    await command('xcrun', ['simctl', 'keychain', this.udid, 'add-root-cert', this.options.certificate], 30_000);
    await this.driver.create({
      capabilities: {
        platformName: 'iOS',
        // Run Safari as a native AUT. browserName=Safari makes XCUITest
        // activate WebKit during createSession, before the simulator has a
        // debuggable page, and can fail the whole session on hosted runners.
        // We open the URL explicitly below and attach to WEBVIEW only after it
        // has been published by Web Inspector.
        'appium:bundleId': 'com.apple.mobilesafari',
        'appium:automationName': 'XCUITest',
        ...(process.env.IOS_PLATFORM_VERSION
          ? { 'appium:platformVersion': process.env.IOS_PLATFORM_VERSION }
          : {}),
        'appium:udid': this.udid,
        'appium:noReset': true,
        'appium:fullReset': false,
        'appium:newCommandTimeout': 1_200,
        'appium:includeSafariInWebviews': true,
        'appium:additionalWebviewBundleIds': ['com.apple.webapp'],
        'appium:autoWebview': false,
        // Allow Web Inspector time to publish a page after simctl openurl.
        'appium:webviewConnectTimeout': 15_000,
        'appium:webviewConnectRetries': 30,
        ...(process.env.IOS_WDA_PREBUILT_PATH
          ? {
            'appium:usePreinstalledWDA': true,
            'appium:prebuiltWDAPath': process.env.IOS_WDA_PREBUILT_PATH,
          }
          : {}),
        // A fresh hosted runner may need several minutes to build and launch WDA.
        'appium:showXcodeLog': true,
        'appium:wdaLaunchTimeout': 300_000,
        'appium:wdaStartupRetries': 1,
        'appium:wdaStartupRetryInterval': 10_000,
      },
      // Keep the client request alive while Appium installs/launches the
      // prebuilt WebDriverAgent and creates the Safari session.
      requestTimeoutMs: 360_000,
      budget: this.budget,
    });
  }

  async openSetupURL(url: string): Promise<void> {
    // Open the URL before asking Appium for WEBVIEW contexts. A Safari browser
    // session starts on about:blank, and querying WebKit before navigation can
    // make hosted XCUITest report a fatal remote-debugger failure.
    const deadline = Date.now() + 30_000;
    let lastError = '';
    while (Date.now() < deadline) {
      try {
        await command('xcrun', ['simctl', 'openurl', this.udid, url], 30_000);
        lastError = '';
        break;
      } catch (error) {
        lastError = error instanceof Error ? error.message : String(error);
        await delay(1_000);
      }
    }
    if (lastError) throw new Error(`IOS_NAVIGATION: could not open setup URL: ${lastError}`);
    await delay(1_500);
    const safariContext = (await this.driver.contextMetadata())
      .find((context) => this.isSafariBundle(context.bundleId) && (!context.url || this.isExpectedOrigin(context.url)))?.id;
    if (!safariContext) return;
    await this.driver.switchContext(safariContext);
    await this.driver.navigate(url);
    await this.driver.switchContext('NATIVE_APP');
  }

  async openSetupURLInInstalledApp(url: string): Promise<void> {
    await this.attachToInstalledView();
    await this.driver.navigate(url);
    await delay(1_000);
  }

  async installFromBrowser(): Promise<void> {
    await this.driver.switchContext('NATIVE_APP').catch(() => undefined);
    const share = await this.driver.findAny([
      accessibility('Share'),
      accessibility('Share button'),
      textLocator('Share'),
    ], 5_000).catch(() => '');
    if (share) {
      await this.driver.click(share);
    } else {
      // Safari's bottom toolbar is visible in the simulator but is not
      // consistently exposed to WDA's accessibility tree on hosted runners.
      // Tap its stable proportional position as a fallback, then continue
      // using semantic locators for the action sheet.
      const size = await this.driver.windowSize().catch(() => ({ width: 402, height: 874 }));
      await this.driver.mobile('tap', { x: size.width / 2, y: size.height * 0.91 });
      await delay(500);
    }
    const add = await this.findNativeScrollable([
      iosLabelContains('Add to Home Screen'),
      textLocator('Add to Home Screen'),
      accessibility('Add to Home Screen'),
      textLocator('Add to Home Screen…'),
      accessibility('Add to Home Screen…'),
    ], 'Add to Home Screen', 30_000);
    await this.driver.click(add);
    const openAsWebApp = await this.driver.findAny([
      iosLabelContains('Open as Web App'),
      textLocator('Open as Web App'),
      accessibility('Open as Web App'),
      textLocator('Open as Web App…'),
      accessibility('Open as Web App…'),
    ], 5_000).catch(() => '');
    if (openAsWebApp) await this.driver.click(openAsWebApp);
    const addButton = await this.driver.findAny([
      textLocator('Add'),
      accessibility('Add'),
    ], 15_000);
    await this.driver.click(addButton);
    await delay(1_500);
  }

  async launchInstalledApp(): Promise<void> {
    this.selectedInstalledContext = '';
    await this.driver.switchContext('NATIVE_APP');
    await this.driver.mobile('pressButton', { name: 'home' });
    await this.ensureSpringBoardForeground();
    await delay(750, this.budget.phaseView('ios-launch', 10_000));
    for (let page = 0; page < 8; page += 1) {
      await this.driver.mobile('swipe', { direction: 'right' });
    }
    for (let page = 0; page < 8; page += 1) {
      await this.ensureSpringBoardForeground();
      const icon = await this.findHittableHomeIcon();
      if (icon) {
        await this.driver.click(icon);
        if (await this.waitForInstalledProvider(5_000)) {
          await this.driver.mobile('activateApp', { bundleId: this.installedBundleId });
          await this.requireInstalledProviderForeground();
          await delay(750);
          await this.attachToInstalledView();
          return;
        }
        await this.driver.mobile('pressButton', { name: 'home' });
      }
      if (page < 7) {
        await this.ensureSpringBoardForeground();
        await this.driver.mobile('swipe', { direction: 'left' });
        await delay(500);
      }
    }
    throw new Error('IOS_CONTEXT: native launch did not identify the installed provider');
  }

  async assertStandalone(origin: string): Promise<RuntimeIdentity> {
    const identity = await this.readRunningIdentity();
    assertStandalone(identity, origin);
    return identity;
  }

  async attachToInstalledView(): Promise<void> {
    const phase = this.budget.phaseView('ios-attachment', 30_000);
    let lastError = '';
    while (!phase.exhausted) {
      phase.assertAvailable('discover installed page');
      if (this.selectedInstalledContext) {
        try {
          await this.driver.switchContext('NATIVE_APP');
          await this.requireInstalledProviderForeground();
          await this.driver.switchContext(this.selectedInstalledContext);
          const url = await this.driver.currentUrl();
          this.lastUrl = url;
          if (this.isExpectedOrigin(url)) {
            await this.validateInstalledDocument();
            return;
          }
          this.selectedInstalledContext = '';
        } catch (error) {
          const detail = error instanceof Error ? error.message : String(error);
          this.diagnostics.record({ phase: 'ios-attachment', operation: 'cached-context', detail });
          throw new Error(`IOS_CONTEXT: cached installed-page selection failed: ${detail}`, { cause: error });
        }
      }
      const contexts = (await this.driver.contextMetadata())
        .filter((context) => context.id !== 'NATIVE_APP')
        .filter((context) => !this.isSafariBundle(context.bundleId));
      if (!contexts.length) {
        lastError = 'installed page metadata is unavailable';
      } else {
        for (const context of contexts) {
          if (context.url && !this.isExpectedOrigin(context.url)) continue;
          await this.driver.switchContext('NATIVE_APP');
          await this.requireInstalledProviderForeground();
          await this.driver.switchContext(context.id);
          const handles = await this.driver.windowHandles().catch(() => []);
          const windows = handles.length ? handles : [''];
          for (const handle of windows) {
            if (handle) await this.driver.switchWindow(handle);
            const url = await this.driver.currentUrl();
            this.lastUrl = url;
            if (!this.isExpectedOrigin(url)) continue;
            await this.validateInstalledDocument();
            this.selectedInstalledContext = context.id;
            return;
          }
        }
        lastError = `no installed page for ${this.origin}`;
      }
      if (this.installedBundleId) {
        phase.recovery('reactivate installed provider');
        await this.driver.switchContext('NATIVE_APP');
        await this.driver.mobile('activateApp', { bundleId: this.installedBundleId });
      }
      await delay(250, phase);
    }
    await this.driver.switchContext('NATIVE_APP').catch(() => undefined);
    throw new Error(`IOS_CONTEXT: no installed Home Screen web context for ${this.origin}: ${lastError}`);
  }

  async readRunningIdentity(): Promise<RuntimeIdentity> {
    await this.attachToInstalledView();
    const identity = await this.driver.execute<RuntimeIdentity>(runtimeScript());
    this.lastIdentity = {
      ...identity,
      nativeProvider: this.installedBundleId ? `ios:${this.installedBundleId}` : undefined,
      nativeActivity: this.lastNativeActivity || undefined,
      nativePid: this.lastNativePid || undefined,
    };
    return this.lastIdentity;
  }

  async readUpdateCompletion(): Promise<UpdateCompletionEvidence> {
    await this.attachToInstalledView();
    this.lastCompletion = await this.driver.execute<UpdateCompletionEvidence>(updateCompletionScript());
    return this.lastCompletion;
  }

  async openFixtureAgent(relayName: string): Promise<void> {
    if (!/^[A-Za-z0-9_.-]+$/u.test(relayName)) throw new Error(`APPIUM_AGENT: invalid fixture relay name ${relayName}`);
    await this.attachToInstalledView();
    const currentUrl = await this.driver.currentUrl().catch(() => '');
    if (currentUrl.includes('#settings')) await this.clickWebText('Back');
    const deadline = Date.now() + 30_000;
    let lastError = '';
    while (Date.now() < deadline) {
      try {
        await this.attachToInstalledView();
        const agent = await this.driver.find(css(`button.agent-open[aria-label="Open mobile-ci on ${relayName}"]`), 2_000);
        // The card remains in the DOM while the relay's inventory reconnects,
        // but its button is disabled until that inventory is ready.
        if ((await this.driver.attribute(agent, 'disabled')) !== null) {
          lastError = `agent ${relayName} is waiting for inventory`;
        } else {
          try {
            await this.driver.click(agent);
          } catch (error) {
            // Chrome-backed web views can report a visible card button as not
            // interactable after a standalone relaunch. Dispatch the same DOM
            // click only after confirming that the matching enabled button is
            // visible.
            const selector = `button.agent-open[aria-label="Open mobile-ci on ${relayName}"]`;
            const result = await this.driver.execute<{ clicked: boolean; reason?: string }>(
              `return (() => {
                const buttons = [...document.querySelectorAll(arguments[0])];
                const button = buttons.find((candidate) => {
                  const rect = candidate.getBoundingClientRect();
                  const style = getComputedStyle(candidate);
                  return !candidate.disabled && rect.width > 0 && rect.height > 0
                    && style.display !== 'none' && style.visibility !== 'hidden';
                });
                if (!button) return { clicked: false, reason: 'no visible enabled agent button' };
                button.scrollIntoView({ block: 'center', inline: 'center' });
                button.click();
                return { clicked: true };
              })();`,
              [selector],
            );
            if (!result.clicked) throw error;
          }
          await delay(1_000);
          return;
        }
      } catch (error) {
        lastError = error instanceof Error ? error.message : String(error);
      }
      await delay(250);
    }
    throw new Error(`APPIUM_AGENT: ${relayName}: ${lastError}`);
  }

  async backgroundApp(): Promise<void> {
    await this.driver.switchContext('NATIVE_APP').catch(() => undefined);
    await this.driver.mobile('pressButton', { name: 'home' });
    await delay(500);
  }

  async relaunchInstalledApp(): Promise<void> {
    await this.launchInstalledApp();
  }

  async terminateInstalledApp(): Promise<void> {
    await this.driver.switchContext('NATIVE_APP').catch(() => undefined);
    const bundleId = this.installedBundleId;
    if (!bundleId) throw new Error('IOS_TERMINATE: the installed Home Screen app did not expose a native provider id');
    await this.driver.mobile('terminateApp', { bundleId });
    await this.driver.mobile('pressButton', { name: 'home' });
  }

  async showKeyboardOnComposer(): Promise<void> {
    await this.attachToInstalledView();
    let composer = await this.driver.find(css('textarea[aria-label="Prompt"]'), 5_000).catch(() => '');
    if (!composer) {
      const open = await this.driver.find(css('button[aria-label^="Open "]'), 30_000);
      await this.driver.click(open);
      composer = await this.driver.find(css('textarea[aria-label="Prompt"]'), 30_000);
    }
    await this.driver.click(composer);
    this.keyboardDraft = 'mobile-device-ci draft';
    await this.driver.sendKeys(composer, this.keyboardDraft);
    await this.waitForKeyboard(true);
  }

  async hideKeyboard(): Promise<void> {
    await this.driver.switchContext('NATIVE_APP');
    await this.driver.mobile('hideKeyboard');
    await this.waitForKeyboard(false);
    await this.attachToInstalledView();
    if (this.keyboardDraft) {
      const value = await this.driver.execute<string>("return document.querySelector('textarea[aria-label=\\\"Prompt\\\"]')?.value || ''");
      if (value !== this.keyboardDraft) throw new Error('IOS_KEYBOARD: draft was not preserved after dismissal');
    }
  }

  async clickWebText(text: string): Promise<void> {
    const deadline = Date.now() + 30_000;
    let lastError = '';
    while (Date.now() < deadline) {
      try {
        await this.attachToInstalledView();
        const element = await this.driver.findAny([buttonText(text), accessibility(text), accessibilityPrefix(text), textLocator(text)], 2_000);
        if ((await this.driver.attribute(element, 'disabled')) === null) {
          await this.driver.click(element);
          return;
        }
        lastError = `${text} is disabled`;
      } catch (error) {
        lastError = error instanceof Error ? error.message : String(error);
      }
      await delay(250);
    }
    throw new Error(`APPIUM_BUTTON: ${text}: ${lastError}`);
  }

  async clickDialogText(dialogId: string, text: string): Promise<void> {
    const deadline = Date.now() + 30_000;
    let lastError = '';
    while (Date.now() < deadline) {
      try {
        await this.attachToInstalledView();
        const buttons = await this.driver.findAll(css(`#${dialogId} button`));
        for (const button of buttons) {
          if ((await this.driver.text(button)).trim() === text) {
            await this.driver.click(button);
            return;
          }
        }
        lastError = `${text} is not visible in ${dialogId}`;
      } catch (error) {
        lastError = error instanceof Error ? error.message : String(error);
      }
      await delay(250);
    }
    throw new Error(`APPIUM_DIALOG_BUTTON: ${dialogId}/${text}: ${lastError}`);
  }

  async setPreference(preference: string): Promise<void> {
    await this.clickWebText('Settings');
    await this.clickWebText(preference === 'state' ? 'By State' : 'Mixed');
  }

  async preferenceValue(): Promise<string> {
    await this.attachToInstalledView();
    return this.driver.execute<string>("return localStorage.getItem('herdr_home_workspace_layout') || ''");
  }

  async captureSanitizedEvidence(name: string): Promise<void> {
    await mkdir(this.outputDir, { recursive: true });
    try {
      const screenshot = Buffer.from(await this.driver.screenshot(), 'base64');
      if (screenshot.byteLength <= 20 * 1024 * 1024) await writeFile(join(this.outputDir, `${name}.png`), screenshot, { mode: 0o600 });
    } catch (error) {
      this.diagnostics.record({ phase: 'evidence', operation: 'screenshot', detail: error instanceof Error ? error.message : String(error) });
    }
    await writeSanitizedJson(join(this.outputDir, `${name}-appium.json`), this.evidenceSnapshot());
    await this.diagnostics.write(join(this.outputDir, `${name}-events.json`));
    const captures: Array<[string, string[]]> = [
      ['simulator-log', ['simctl', 'spawn', this.udid, 'log', 'show', '--last', '10m', '--style', 'compact']],
      ['webkit-safari', ['simctl', 'spawn', this.udid, 'log', 'show', '--last', '10m', '--style', 'compact', '--predicate', 'process CONTAINS[c] "WebKit" OR process CONTAINS[c] "Safari"']],
      ['apps', ['simctl', 'listapps', this.udid]],
      ['wda', ['simctl', 'spawn', this.udid, 'launchctl', 'print', 'system']],
    ];
    for (const [suffix, args] of captures) {
      const output = await commandOutput('xcrun', args, 20_000).catch((error) => error instanceof Error ? error.message : String(error));
      await writeBoundedText(join(this.outputDir, `${name}-ios-${suffix}.log`), output);
    }
  }

  evidenceSnapshot(): Record<string, unknown> {
    return {
      platform: this.name,
      device: this.udid,
      origin: this.origin,
      installedBundleId: this.installedBundleId,
      selectedInstalledContext: this.selectedInstalledContext,
      lastUrl: this.lastUrl,
      lastIdentity: this.lastIdentity,
      lastCompletion: this.lastCompletion,
      nativeActivity: this.lastNativeActivity,
      nativePid: this.lastNativePid,
      driver: this.driver.snapshot(),
      events: this.diagnostics.snapshot(),
    };
  }

  async stopOwnedResources(): Promise<void> {
    await this.driver.close();
  }

  private async findNativeScrollable(locators: Locator[], description: string, timeoutMs: number): Promise<string> {
    const deadline = Date.now() + timeoutMs;
    let lastError = '';
    let scrolls = 0;
    while (Date.now() < deadline) {
      try {
        return await this.driver.findAny(locators, Math.min(1_500, Math.max(1, deadline - Date.now())));
      } catch (error) {
        lastError = error instanceof Error ? error.message : String(error);
      }
      if (scrolls < 8) {
        try {
          await this.driver.mobile('scroll', { direction: 'up', distance: 0.75 });
        } catch (error) {
          lastError = error instanceof Error ? error.message : String(error);
          await this.driver.mobile('swipe', { direction: 'up' }).catch(() => undefined);
        }
        scrolls += 1;
      }
      await delay(250);
    }
    throw new Error(`IOS_SHARE: ${description}: ${lastError}`);
  }

  private async findHittableHomeIcon(): Promise<string> {
    const locators = [
      accessibility('Herdr Mobile Relay'),
      textLocator('Herdr Mobile Relay'),
      accessibility('Herdr Relay'),
      textLocator('Herdr Relay'),
      {
        using: 'xpath',
        value: "//*[@name='Home screen icons']//*[contains(@name, 'Herdr Mobile Relay') or contains(@name, 'Herdr Relay') or contains(@label, 'Herdr Mobile Relay') or contains(@label, 'Herdr Relay')]",
      },
    ];
    for (const locator of locators) {
      const elements = await this.driver.findAll(locator).catch(() => []);
      for (const element of elements) {
        const hittable = await this.driver.attribute(element, 'hittable').catch(() => null);
        if (hittable === 'true') return element;
        const visible = await this.driver.attribute(element, 'visible').catch(() => null);
        if (hittable === null && visible === 'true') return element;
      }
    }
    return '';
  }

  private async waitForInstalledProvider(timeoutMs: number): Promise<boolean> {
    const phase = this.budget.phaseView('ios-provider', timeoutMs);
    while (!phase.exhausted) {
      phase.assertAvailable('discover installed provider');
      await this.driver.switchContext('NATIVE_APP');
      const appInfo = await this.driver.activeAppInfo();
      const bundleId = String(appInfo?.bundleId || appInfo?.bundleID || '');
      if (bundleId && !this.isSafariBundle(bundleId) && !/springboard/iu.test(bundleId)) {
        this.installedBundleId = bundleId;
        return true;
      }
      await delay(250, phase);
    }
    return false;
  }

  private async ensureSpringBoardForeground(): Promise<void> {
    await this.driver.switchContext('NATIVE_APP');
    const info = await this.driver.activeAppInfo();
    const bundleId = String(info?.bundleId || info?.bundleID || '');
    if (!/springboard/iu.test(bundleId)) {
      await this.driver.mobile('activateApp', { bundleId: 'com.apple.springboard' });
    }
    const foreground = await this.driver.activeAppInfo();
    const active = String(foreground?.bundleId || foreground?.bundleID || '');
    if (!/springboard/iu.test(active)) throw new Error(`IOS_NATIVE: SpringBoard is not foreground (${active || 'unknown'})`);
  }

  private async requireInstalledProviderForeground(): Promise<void> {
    if (!this.installedBundleId) throw new Error('IOS_CONTEXT: installed provider identity is unavailable');
    const info = await this.driver.activeAppInfo();
    const active = String(info?.bundleId || info?.bundleID || '');
    this.lastNativeActivity = String(info?.activity || info?.appActivity || '');
    this.lastNativePid = String(info?.pid || '');
    if (active !== this.installedBundleId) throw new Error(`IOS_CONTEXT: installed provider ${this.installedBundleId} is not foreground (${active || 'unknown'})`);
  }

  private async validateInstalledDocument(): Promise<void> {
    const document = await this.driver.execute<{ origin: string; standalone: boolean }>(`return {
      origin: location.origin,
      standalone: window.matchMedia('(display-mode: standalone)').matches || navigator.standalone === true,
    };`);
    if (document.origin !== this.origin) throw new Error(`IOS_CONTEXT: document origin ${document.origin} is not ${this.origin}`);
    if (!document.standalone) throw new Error('IOS_CONTEXT: selected page is not standalone');
  }

  private isSafariBundle(bundleId?: string): boolean {
    return Boolean(bundleId && /safari|safariviewservice/iu.test(bundleId));
  }

  private isExpectedOrigin(value: string): boolean {
    try {
      return new URL(value).origin === new URL(this.origin).origin;
    } catch {
      return false;
    }
  }

  private async waitForKeyboard(expected: boolean): Promise<void> {
    const phase = this.budget.phaseView('ios-keyboard', 10_000);
    let last = '';
    while (!phase.exhausted) {
      phase.assertAvailable(`keyboard ${expected ? 'show' : 'hide'}`);
      await this.driver.switchContext('NATIVE_APP');
      const elements = await this.driver.findAll({ using: 'class name', value: 'XCUIElementTypeKeyboard' }).catch(() => []);
      let visible = false;
      for (const element of elements) {
        const rect = await this.driver.elementRect(element).catch(() => ({ x: 0, y: 0, width: 0, height: 0 }));
        const shown = await this.driver.attribute(element, 'visible').catch(() => null);
        if (rect.width > 0 && rect.height > 0 && shown !== 'false') visible = true;
      }
      last = `visible=${visible}`;
      if (visible === expected) return;
      await delay(250, phase);
    }
    throw new Error(`IOS_KEYBOARD: expected ${expected ? 'visible' : 'hidden'} native keyboard (${last})`);
  }

  private async currentForegroundPackage(): Promise<string> {
    const info = await this.driver.activeAppInfo();
    return String(info?.bundleId || info?.bundleID || '');
  }
}
