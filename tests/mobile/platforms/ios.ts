import { mkdir, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { assertStandalone, type RuntimeIdentity } from '../support/oracle';
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
} from '../support/webdriver';
import { runtimeScript, updateCompletionScript, type MobilePlatform, type PlatformOptions, type UpdateCompletionEvidence } from './types';

export class IOSPlatform implements MobilePlatform {
  readonly name = 'ios' as const;
  readonly driver: AppiumClient;
  private readonly udid: string;
  private readonly origin: string;
  private readonly outputDir: string;
  private installedBundleId = '';

  constructor(private readonly options: PlatformOptions) {
    this.udid = options.deviceId || process.env.IOS_SIMULATOR_UDID || '';
    this.origin = options.origin.replace(/\/$/, '');
    this.outputDir = options.outputDir;
    this.driver = new AppiumClient(options.appiumUrl);
  }

  async startFreshDevice(): Promise<void> {
    if (!this.udid) throw new Error('IOS_TARGET: IOS_SIMULATOR_UDID is required');
    await requireOwnedDevice('ios', this.udid);
    const available = await commandOutput('xcrun', ['simctl', 'list', 'devices', 'available']);
    if (!available.includes(this.udid)) throw new Error(`IOS_TARGET: simulator ${this.udid} is not an available simulator`);
    if (process.env.MOBILE_RESET_DEVICE === '1') {
      await command('xcrun', ['simctl', 'shutdown', this.udid]).catch(() => undefined);
      await command('xcrun', ['simctl', 'erase', this.udid], 120_000);
    }
    await command('xcrun', ['simctl', 'boot', this.udid]).catch(() => undefined);
    await command('xcrun', ['simctl', 'bootstatus', this.udid, '-b'], 300_000);
    await command('xcrun', ['simctl', 'keychain', this.udid, 'add-root-cert', this.options.certificate], 30_000);
    await this.driver.create({
      capabilities: {
        platformName: 'iOS',
        browserName: 'Safari',
        'appium:automationName': 'XCUITest',
        'appium:udid': this.udid,
        'appium:noReset': true,
        'appium:fullReset': false,
        'appium:newCommandTimeout': 1_200,
        'appium:includeSafariInWebviews': true,
        'appium:autoWebview': false,
        'appium:usePrebuiltWDA': false,
        ...(process.env.IOS_WDA_BOOTSTRAP_PATH
          ? {
            'appium:useXctestrunFile': true,
            'appium:bootstrapPath': process.env.IOS_WDA_BOOTSTRAP_PATH,
            'appium:derivedDataPath': process.env.IOS_WDA_DERIVED_DATA_PATH,
          }
          : {}),
        // A fresh hosted runner may need several minutes to build and launch WDA.
        'appium:showXcodeLog': true,
        'appium:wdaLaunchTimeout': 300_000,
        'appium:wdaStartupRetries': 1,
        'appium:wdaStartupRetryInterval': 10_000,
      },
      // The first XCUITest session builds WebDriverAgent on the hosted runner.
      // Keep the client request alive for that one-time build.
      requestTimeoutMs: 360_000,
    });
  }

  async openSetupURL(url: string): Promise<void> {
    const safariContext = (await this.driver.contexts().catch(() => []))
      .find((context) => /^WEBVIEW_/u.test(context));
    if (safariContext) {
      try {
        // Keep Appium attached to the same Safari page. Using simctl alone can
        // leave the Web Inspector session on its initial about:blank page.
        await this.driver.switchContext(safariContext);
        await this.driver.navigate(url);
        await delay(1_500);
        await this.driver.switchContext('NATIVE_APP');
        return;
      } catch {
        await this.driver.switchContext('NATIVE_APP').catch(() => undefined);
      }
    }
    await command('xcrun', ['simctl', 'openurl', this.udid, url], 30_000);
    await delay(1_500);
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
    const add = await this.driver.findAny([
      textLocator('Add to Home Screen'),
      accessibility('Add to Home Screen'),
    ], 30_000);
    await this.driver.click(add);
    const openAsWebApp = await this.driver.findAny([
      textLocator('Open as Web App'),
      accessibility('Open as Web App'),
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
    await this.driver.switchContext('NATIVE_APP').catch(() => undefined);
    await this.driver.mobile('pressButton', { name: 'home' });
    const icon = await this.driver.findAny([
      accessibility('Herdr Mobile Relay'),
      textLocator('Herdr Mobile Relay'),
      accessibility('Herdr Relay'),
      textLocator('Herdr Relay'),
    ], 30_000);
    await this.driver.click(icon);
    await this.identifyInstalledProvider();
    await this.attachToInstalledView();
  }

  async assertStandalone(origin: string): Promise<RuntimeIdentity> {
    const identity = await this.readRunningIdentity();
    assertStandalone(identity, origin);
    return identity;
  }

  async attachToInstalledView(): Promise<void> {
    const deadline = Date.now() + 30_000;
    while (Date.now() < deadline) {
      const contexts = await this.driver.contexts().catch(() => []);
      for (const context of contexts.filter((value) => value !== 'NATIVE_APP' && !/safari/iu.test(value))) {
        await this.driver.switchContext(context).catch(() => undefined);
        const url = await this.driver.currentUrl().catch(() => '');
        if (url === this.origin || url.startsWith(`${this.origin}/`)) {
          return;
        }
      }
      await delay(250);
    }
    await this.driver.switchContext('NATIVE_APP').catch(() => undefined);
    throw new Error(`IOS_CONTEXT: no installed Home Screen web context for ${this.origin}`);
  }

  async readRunningIdentity(): Promise<RuntimeIdentity> {
    await this.attachToInstalledView();
    const identity = await this.driver.execute<RuntimeIdentity>(runtimeScript());
    return { ...identity, nativeProvider: this.installedBundleId ? `ios:${this.installedBundleId}` : undefined };
  }

  async readUpdateCompletion(): Promise<UpdateCompletionEvidence> {
    await this.attachToInstalledView();
    return this.driver.execute<UpdateCompletionEvidence>(updateCompletionScript());
  }

  async openFixtureAgent(relayName: string): Promise<void> {
    if (!/^[A-Za-z0-9_.-]+$/u.test(relayName)) throw new Error(`APPIUM_AGENT: invalid fixture relay name ${relayName}`);
    await this.attachToInstalledView();
    const currentUrl = await this.driver.currentUrl().catch(() => '');
    if (currentUrl.includes('#settings')) await this.clickWebText('Back');
    await this.attachToInstalledView();
    const agent = await this.driver.find(css(`button.agent-open[aria-label="Open mobile-ci on ${relayName}"]`), 30_000);
    await this.driver.click(agent);
    await delay(1_000);
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
    await this.driver.sendKeys(composer, 'mobile-device-ci draft');
    await this.driver.switchContext('NATIVE_APP');
    await this.driver.findAny([
      accessibility('Done'),
      accessibility('Return'),
      accessibility('return'),
      textLocator('Done'),
    ], 10_000);
    await this.attachToInstalledView();
  }

  async hideKeyboard(): Promise<void> {
    await this.driver.switchContext('NATIVE_APP');
    await this.driver.mobile('hideKeyboard').catch(() => undefined);
    await this.attachToInstalledView();
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
    const screenshot = Buffer.from(await this.driver.screenshot(), 'base64');
    if (screenshot.byteLength > 20 * 1024 * 1024) throw new Error('DIAGNOSTIC_LIMIT: screenshot exceeds the limit');
    await writeFile(join(this.outputDir, `${name}.png`), screenshot, { mode: 0o600 });
  }

  async stopOwnedResources(): Promise<void> {
    await this.driver.close();
  }

  private async identifyInstalledProvider(): Promise<void> {
    const deadline = Date.now() + 30_000;
    while (Date.now() < deadline) {
      await this.driver.switchContext('NATIVE_APP').catch(() => undefined);
      const appInfo = await this.driver.mobile('activeAppInfo').catch(() => null) as Record<string, unknown> | null;
      const bundleId = String(appInfo?.bundleId || appInfo?.bundleID || '');
      if (bundleId && !/springboard|safari/iu.test(bundleId)) {
        this.installedBundleId = bundleId;
        return;
      }
      await delay(250);
    }
    throw new Error('IOS_CONTEXT: native launch did not identify the installed provider');
  }
}
