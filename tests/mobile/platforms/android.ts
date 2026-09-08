import { X509Certificate } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { basename, join } from 'node:path';
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
  type Locator,
} from '../support/webdriver';
import { runtimeScript, updateCompletionScript, type MobilePlatform, type PlatformOptions, type UpdateCompletionEvidence } from './types';

function androidShellQuote(value: string): string {
  return `'${value.replaceAll("'", "'\"'\"'")}'`;
}

export function androidOpenUrlArgs(serial: string, url: string): string[] {
  const remoteCommand = ['am', 'start', '-a', 'android.intent.action.VIEW', '-d', url, 'com.android.chrome']
    .map(androidShellQuote)
    .join(' ');
  return ['-s', serial, 'shell', remoteCommand];
}

export class AndroidPlatform implements MobilePlatform {
  readonly name = 'android' as const;
  readonly driver: AppiumClient;
  private readonly serial: string;
  private readonly origin: string;
  private readonly outputDir: string;
  private installedPackage = '';

  constructor(private readonly options: PlatformOptions) {
    this.serial = options.deviceId || process.env.ANDROID_SERIAL || '';
    this.origin = options.origin.replace(/\/$/, '');
    this.outputDir = options.outputDir;
    this.driver = new AppiumClient(options.appiumUrl);
  }

  async startFreshDevice(): Promise<void> {
    if (!/^emulator-\d+$/.test(this.serial)) throw new Error('ANDROID_TARGET: refusing a non-emulator or ambiguous device');
    await requireOwnedDevice('android', this.serial);
    const devices = await commandOutput(process.env.ADB || 'adb', ['devices']);
    const matching = devices.split(/\r?\n/).filter((line) => line.startsWith(`${this.serial}\t`));
    if (matching.length !== 1 || !matching[0].endsWith('\tdevice')) throw new Error(`ANDROID_TARGET: ${this.serial} is not the only ready emulator`);
    const adb = process.env.ADB || 'adb';
    await command(adb, ['-s', this.serial, 'wait-for-device'], 60_000);
    await command(adb, ['-s', this.serial, 'shell', 'input', 'keyevent', 'KEYCODE_HOME']);
    await command(adb, ['-s', this.serial, 'shell', 'pm', 'clear', 'com.android.chrome']).catch(() => undefined);
    const installedPackages = await commandOutput(adb, ['-s', this.serial, 'shell', 'pm', 'list', 'packages']);
    for (const packageName of installedPackages.split(/\r?\n/u).map((line) => line.replace(/^package:/u, '').trim()).filter((value) => /webapk|herdr/iu.test(value))) {
      await command(adb, ['-s', this.serial, 'uninstall', packageName]).catch(() => undefined);
    }
    await this.driver.create({
      capabilities: {
        platformName: 'Android',
        browserName: 'Chrome',
        'appium:automationName': 'UiAutomator2',
        'appium:udid': this.serial,
        'appium:noReset': true,
        'appium:fullReset': false,
        'appium:newCommandTimeout': 1_200,
        'appium:skipDeviceInitialization': false,
        'appium:skipServerInstallation': false,
      },
      requestTimeoutMs: 60_000,
    });
    await this.installCertificate();
  }

  async openSetupURL(url: string): Promise<void> {
    await command(process.env.ADB || 'adb', androidOpenUrlArgs(this.serial, url), 30_000);
    await delay(1_000);
  }

  async openSetupURLInInstalledApp(url: string): Promise<void> {
    await this.attachToInstalledView();
    await this.driver.navigate(url);
    await delay(1_000);
  }

  async installFromBrowser(): Promise<void> {
    await this.driver.switchContext('NATIVE_APP').catch(() => undefined);
    const menu = await this.driver.findAny([
      accessibility('More options'),
      textLocator('More options'),
    ], 30_000);
    await this.driver.click(menu);
    const install = await this.driver.findAny([
      textLocator('Install app'),
      accessibility('Install app'),
    ], 15_000);
    await this.driver.click(install);
    const confirm = await this.driver.findAny([
      textLocator('Add'),
      textLocator('Install'),
    ], 15_000);
    await this.driver.click(confirm);
    await delay(1_500);
    await command(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'input', 'keyevent', 'KEYCODE_HOME']);
  }

  async launchInstalledApp(): Promise<void> {
    await this.driver.switchContext('NATIVE_APP').catch(() => undefined);
    await command(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'input', 'keyevent', 'KEYCODE_HOME']);
    const icon = await this.driver.findAny([
      textLocator('Herdr Mobile Relay'),
      accessibility('Herdr Mobile Relay'),
      textLocator('Herdr Relay'),
      accessibility('Herdr Relay'),
    ], 30_000);
    await this.driver.click(icon);
    await delay(1_000);
    this.installedPackage = await this.currentForegroundPackage();
    await this.attachToInstalledView();
  }

  async assertStandalone(origin: string): Promise<RuntimeIdentity> {
    const identity = await this.readRunningIdentity();
    assertStandalone(identity, origin);
    return identity;
  }

  async attachToInstalledView(): Promise<void> {
    const deadline = Date.now() + 30_000;
    const expectedOrigin = this.origin;
    while (Date.now() < deadline) {
      const contexts = await this.driver.contexts().catch(() => []);
      for (const context of contexts.filter((value) => value !== 'NATIVE_APP')) {
        await this.driver.switchContext(context).catch(() => undefined);
        const url = await this.driver.currentUrl().catch(() => '');
        if (url.startsWith(`${expectedOrigin}/`) || url === expectedOrigin) return;
      }
      await delay(250);
    }
    await this.driver.switchContext('NATIVE_APP').catch(() => undefined);
    throw new Error(`ANDROID_CONTEXT: no installed web context for ${expectedOrigin}`);
  }

  async readRunningIdentity(): Promise<RuntimeIdentity> {
    await this.attachToInstalledView();
    const identity = await this.driver.execute<RuntimeIdentity>(runtimeScript());
    return { ...identity, nativeProvider: this.installedPackage ? `android:${this.installedPackage}` : undefined };
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
    await command(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'input', 'keyevent', 'KEYCODE_HOME']);
    await delay(500);
  }

  async relaunchInstalledApp(): Promise<void> {
    await this.launchInstalledApp();
  }

  async terminateInstalledApp(): Promise<void> {
    await this.driver.switchContext('NATIVE_APP').catch(() => undefined);
    const packageName = this.installedPackage || await this.currentForegroundPackage();
    if (!packageName || packageName === 'com.android.launcher3' || packageName === 'com.google.android.apps.nexuslauncher') {
      throw new Error('ANDROID_TERMINATE: could not identify the installed PWA process');
    }
    await command(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'am', 'force-stop', packageName]);
    await command(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'input', 'keyevent', 'KEYCODE_HOME']);
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
    const state = await commandOutput(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'dumpsys', 'input_method']);
    if (!/mInputShown=true|isInputShown=true/u.test(state)) throw new Error('ANDROID_KEYBOARD: software keyboard did not become visible');
  }

  async hideKeyboard(): Promise<void> {
    await command(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'input', 'keyevent', 'KEYCODE_BACK']);
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

  private async installCertificate(): Promise<void> {
    const adb = process.env.ADB || 'adb';
    const remote = '/sdcard/Download/herdr-mobile-ci-ca.crt';
    const remoteName = basename(remote);
    const commonName = await this.certificateCommonName();

    await command(adb, ['-s', this.serial, 'push', this.options.certificate, remote], 30_000);
    await command(adb, ['-s', this.serial, 'shell', 'am', 'start', '-a', 'android.settings.SECURITY_SETTINGS'], 30_000);
    await this.driver.switchContext('NATIVE_APP').catch(() => undefined);
    await this.waitForForegroundPackage('com.android.settings');
    await delay(500);

    // Android 11+ only permits CA installation when the user starts it from
    // Settings. The generic credential intent is deliberately rejected and
    // cannot select the file pushed above on the Android 35 image.
    await this.clickNative([
      accessibility('More security settings'),
      accessibility('More security & privacy'),
      accessibility('More security and privacy'),
      textLocator('More security settings'),
      textLocator('More security & privacy'),
      textLocator('More security and privacy'),
    ], 'More security settings');
    await this.clickNative([
      accessibility('Encryption & credentials'),
      textLocator('Encryption & credentials'),
      textLocator('Encryption & Credentials'),
    ], 'Encryption & credentials');
    await this.clickNative([
      accessibility('Install a certificate'),
      accessibility('Install from device storage'),
      accessibility('Install from storage'),
      textLocator('Install a certificate'),
      textLocator('Install from device storage'),
      textLocator('Install from storage'),
    ], 'Install a certificate');
    await this.clickNative([
      accessibility('CA certificate'),
      textLocator('CA certificate'),
      textLocator('CA Certificate'),
    ], 'CA certificate');
    await this.clickNative([
      accessibility('Install anyway'),
      accessibility('INSTALL ANYWAY'),
      textLocator('Install anyway'),
      textLocator('INSTALL ANYWAY'),
    ], 'Install anyway');

    // The supported flow now opens DocumentsUI. Explicitly open Downloads and
    // choose the pushed file; do not continue after a missing control.
    await this.clickNative([
      accessibility('Show roots'),
      accessibility('Open navigation drawer'),
      accessibility('Open navigation'),
    ], 'certificate picker navigation');
    await this.clickNative([
      accessibility('Downloads'),
      textLocator('Downloads'),
    ], 'Downloads');
    await this.clickNative([
      accessibility(remoteName),
      textLocator(remoteName),
    ], remoteName);
    // Android 15 installs a CA certificate immediately after the file is
    // selected; there is no certificate-name dialog or Done button.
    await delay(1_000);
    await this.verifyCertificate(remoteName, commonName);
    await command(adb, ['-s', this.serial, 'shell', 'rm', '-f', remote], 30_000);
  }

  private async clickNative(locators: Locator[], description: string, timeoutMs = 30_000): Promise<void> {
    try {
      const element = await this.findNative(locators, timeoutMs);
      await this.driver.click(element);
    } catch (error) {
      throw new Error(`ANDROID_CERTIFICATE: ${description}: ${error instanceof Error ? error.message : String(error)}`, { cause: error });
    }
  }

  private async findNative(locators: Locator[], timeoutMs: number): Promise<string> {
    const deadline = Date.now() + timeoutMs;
    let lastError = '';
    let scrolls = 0;
    const size = await this.driver.windowSize().catch(() => ({ width: 1_080, height: 2_400 }));
    while (Date.now() < deadline) {
      try {
        return await this.driver.findAny(locators, Math.min(2_000, Math.max(1, deadline - Date.now())));
      } catch (error) {
        lastError = error instanceof Error ? error.message : String(error);
      }
      if (scrolls >= 8) {
        await delay(250);
        continue;
      }
      await this.driver.mobile('scrollGesture', {
        left: 0,
        top: 100,
        width: size.width,
        height: Math.max(1, size.height - 200),
        direction: 'down',
        percent: 0.75,
      }).catch((error) => {
        lastError = error instanceof Error ? error.message : String(error);
      });
      scrolls += 1;
      await delay(250);
    }
    throw new Error(`APPIUM_NATIVE: ${lastError}`);
  }

  private async waitForForegroundPackage(packageName: string, timeoutMs = 30_000): Promise<void> {
    const deadline = Date.now() + timeoutMs;
    let current = '';
    while (Date.now() < deadline) {
      current = await this.currentForegroundPackage();
      if (current === packageName) return;
      await delay(250);
    }
    throw new Error(`ANDROID_CERTIFICATE: expected ${packageName} in foreground, found ${current || 'none'}`);
  }

  private async certificateCommonName(): Promise<string> {
    const certificate = new X509Certificate(await readFile(this.options.certificate));
    const commonName = certificate.subject.match(/CN\s*=\s*([^,\n/]+)/u)?.[1]?.trim();
    if (!commonName) throw new Error('ANDROID_CERTIFICATE: supplied certificate has no common name');
    return commonName;
  }

  private async verifyCertificate(certificateName: string, commonName: string): Promise<void> {
    const adb = process.env.ADB || 'adb';
    await command(adb, ['-s', this.serial, 'shell', 'am', 'start', '-a', 'com.android.settings.TRUSTED_CREDENTIALS_USER'], 30_000);
    await this.driver.switchContext('NATIVE_APP').catch(() => undefined);
    await this.waitForForegroundPackage('com.android.settings');
    await delay(500);
    const nameWithoutExtension = certificateName.replace(/\.[^.]+$/u, '');
    await this.driver.findAny([
      accessibility(commonName),
      textLocator(commonName),
      accessibility(certificateName),
      textLocator(certificateName),
      accessibility(nameWithoutExtension),
      textLocator(nameWithoutExtension),
    ], 20_000);

    // Also exercise the actual fixture HTTPS endpoint before the certificate
    // file is removed. The trusted-credentials list alone does not prove that
    // Chrome can build the chain used by the mobile harness.
    await command(adb, androidOpenUrlArgs(this.serial, `${this.origin}/version.json`), 30_000);
    await delay(1_000);
    await this.attachToInstalledView();
    const source = await this.driver.pageSource();
    if (/ERR_CERT|NET::ERR|privacy error|not private/iu.test(source)) {
      throw new Error('ANDROID_CERTIFICATE: fixture HTTPS endpoint is not trusted');
    }
  }

  private async currentForegroundPackage(): Promise<string> {
    const output = await commandOutput(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'dumpsys', 'activity', 'activities']);
    return output.match(/(?:mResumedActivity|ResumedActivity): ActivityRecord\{[^}]+\s([A-Za-z0-9_.]+)\//)?.[1] || '';
  }
}
