import type { DiagnosticRecorder } from '../support/diagnostics';
import type { PhaseBudget } from '../support/budget';
import type { AppiumClient } from '../support/webdriver';
import type { RuntimeIdentity } from '../support/oracle';

export interface UpdateCompletionEvidence {
  phoneRequired: boolean;
  phoneAcknowledged: boolean;
  phoneState: string;
  visibleCompletion: boolean;
  rawPlanPresent: boolean;
}

export interface PlatformOptions {
  origin: string;
  appiumUrl: string;
  outputDir: string;
  certificate: string;
  setupUrl: string;
  deviceId?: string;
  budget?: PhaseBudget;
  diagnostics?: DiagnosticRecorder;
}

export interface MobilePlatform {
  readonly name: 'android' | 'ios';
  readonly driver: AppiumClient;
  startFreshDevice(): Promise<void>;
  openSetupURL(url: string): Promise<void>;
  openSetupURLInInstalledApp(url: string): Promise<void>;
  installFromBrowser(): Promise<void>
  launchInstalledApp(): Promise<void>;
  assertStandalone(origin: string): Promise<RuntimeIdentity>;
  attachToInstalledView(timeoutMs?: number): Promise<void>;
  readRunningIdentity(): Promise<RuntimeIdentity>;
  readUpdateCompletion(): Promise<UpdateCompletionEvidence>;
  openFixtureAgent(relayName: string): Promise<void>;
  backgroundApp(): Promise<void>;
  relaunchInstalledApp(): Promise<void>;
  terminateInstalledApp(): Promise<void>;
  showKeyboardOnComposer(): Promise<void>;
  hideKeyboard(): Promise<void>;
  clickWebText(text: string): Promise<void>;
  clickDialogText(dialogId: string, text: string): Promise<void>;
  setPreference(preference: string): Promise<void>;
  preferenceValue(): Promise<string>;
  captureSanitizedEvidence(name: string): Promise<void>;
  evidenceSnapshot(): Record<string, unknown>;
  stopOwnedResources(): Promise<void>;
}

export function runtimeScript(): string {
  return `return (() => {
    const script = document.querySelector('script[type="module"][src*="/assets/app"]');
    const style = document.querySelector('link[rel="stylesheet"][href*="/assets/app"]');
    let metadata = {};
    try {
      const request = new XMLHttpRequest();
      request.open('GET', '/version.json', false);
      request.send();
      if (request.status >= 200 && request.status < 300) metadata = JSON.parse(request.responseText || '{}');
    } catch {}
    const buildEntry = /^\\/builds\\/[^/]+\\/index\\.html$/.test(location.pathname)
      ? location.pathname
      : '';
    const versionMatch = [...document.querySelectorAll('main, p, small')]
      .map((node) => (node.textContent || '').match(/Phone app version\\s+(\\d+\\.\\d+\\.\\d+)/))
      .find((match) => match);
    const version = versionMatch?.[1] || String(metadata.release_version || metadata.version || '');
    const identityNode = document.querySelector('[data-app-assets]');
    const assetsText = identityNode?.getAttribute('data-app-assets') || document.documentElement.dataset.appAssets || '';
    const buildText = identityNode?.getAttribute('data-app-build') || document.documentElement.dataset.appBuild || '';
    const observedAssets = Number(assetsText) || Number(buildEntry.match(/\\/builds\\/[^/]+-(\\d+)-/)?.[1] || metadata.assets || 0);
    const observedBuild = buildText || (buildEntry.match(/-([a-f0-9]{16,64})\\/index\\.html$/)?.[1] || String(metadata.build || ''));
    const observedEntry = buildEntry || String(metadata.entry || (version && observedAssets && observedBuild.length >= 16
      ? '/builds/' + version + '-' + observedAssets + '-' + observedBuild.slice(0, 16) + '/index.html'
      : ''));
    const standalone = window.matchMedia('(display-mode: standalone)').matches
      || navigator.standalone === true;
    const iosHomeScreen = /iPhone|iPad|iPod/u.test(navigator.userAgent);
    return {
      url: location.href,
      origin: location.origin,
      standalone,
      provider: standalone ? (iosHomeScreen ? 'ios-home-screen' : 'android-standalone') : 'browser',
      navigationId: String(performance.timeOrigin),
      version,
      assets: observedAssets,
      build: observedBuild,
      buildFromApplication: Boolean(buildText),
      entry: observedEntry,
      script: script ? new URL(script.getAttribute('src') || '', location.origin).pathname : '',
      style: style ? new URL(style.getAttribute('href') || '', location.origin).pathname : '',
      requiredAssetsReady: document.documentElement.dataset.herdrLoadFailed !== '1'
        && document.documentElement.dataset.herdrLoadTimedOut !== '1'
        && Boolean(style?.sheet || document.documentElement.dataset.herdrCssReady === '1'),
      requiredAssetFailure: document.documentElement.dataset.herdrLoadFailed === '1'
        || document.documentElement.dataset.herdrLoadTimedOut === '1',
      failureUiVisible: /Herdr could not load|Try again|Phone app failed to load/u.test(document.body?.innerText || ''),
      applicationInitialized: Boolean(document.getElementById('app')?.childNodes.length),
    };
  })()`;
}

export function updateCompletionScript(): string {
  return `return (() => {
    const rawPlan = sessionStorage.getItem('herdr_update_progress') || '';
    let plan = {};
    try { plan = rawPlan ? JSON.parse(rawPlan) : {}; } catch {}
    const text = document.body?.innerText || '';
    return {
      phoneRequired: plan && plan.phoneAppRequired === true,
      phoneAcknowledged: plan && plan.phoneAcknowledged === true,
      phoneState: String(plan?.phoneState || ''),
      visibleCompletion: /Update complete|Phone app v[^\\n]+ loaded/u.test(text),
      rawPlanPresent: Boolean(rawPlan),
    };
  })()`;
}
