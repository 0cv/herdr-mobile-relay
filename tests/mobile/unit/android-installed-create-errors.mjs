import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import {createHash} from 'node:crypto';
import {pathToFileURL, fileURLToPath} from 'node:url';

const retentionFailure = 'Original create error must be retained before cleanup';
async function main() {
  const [mode, image] = process.argv.slice(2);
  assert.ok(['behavior', 'retention'].includes(mode) && ['pristine', 'patched'].includes(image));
  assert.equal(process.env.HERDR_OWNED_INSTALLER_FIXTURE, '1');
  assert.ok(process.env.APPIUM_HOME && path.isAbsolute(process.env.APPIUM_HOME));
  const home = fs.realpathSync(process.env.APPIUM_HOME);
  const directory = new URL('../android-appium/', import.meta.url);
  const runtime = JSON.parse(fs.readFileSync(new URL('runtime-manifest.json', directory)));
  const manifest = JSON.parse(fs.readFileSync(new URL('producer-manifest.json', directory)));
  const parent = pathToFileURL(path.join(home, 'package.json')).href;
  const entry = import.meta.resolve('appium-uiautomator2-driver', parent);
  const root = path.join(home, runtime.uiautomator2.root);
  assert.equal(fileURLToPath(entry), path.join(root, runtime.uiautomator2.entry));
  const driverURL = new URL('./driver.js', entry);
  assert.equal(fs.realpathSync(fileURLToPath(driverURL)), fileURLToPath(driverURL));
  for (const [file, expected] of Object.entries(manifest.files).filter(([, value]) => value.package === 'uiautomator2')) {
    const actual = createHash('sha256').update(fs.readFileSync(path.join(root, file))).digest('hex');
    assert.equal(actual, expected[image === 'pristine' ? 'before' : 'after']);
  }
  const {AndroidUiautomator2Driver: Driver} = await import(entry);
  const {AndroidUiautomator2Driver: DirectDriver} = await import(driverURL.href);
  assert.equal(Driver, DirectDriver);
  const {BaseDriver} = await import(import.meta.resolve('appium/driver.js', driverURL.href));
  const {sessionClaimHandler} = await import(new URL('./session-claim-handler.js', entry));
  let active;
  BaseDriver.prototype.createSession = async function () {
    active.native('base:create');
    this.sessionId = 'synthetic-session';
    this.caps = {platformName: 'Android'};
    return [this.sessionId, this.caps];
  };
  BaseDriver.prototype.deleteSession = async function () { active.native('base:delete'); };
  sessionClaimHandler.registerActiveSession = async () => active.native('claim:register');
  sessionClaimHandler.claimSessionUdid = async () => active.native('claim:acquire');
  sessionClaimHandler.unregisterActiveSession = () => active.native('claim:release');
  const createTrace = ['base:create', 'native:device-info', 'claim:register', 'claim:acquire', 'native:adb', 'native:prepare', 'native:create'];
  const cleanupTrace = ['claim:release', 'native:stop-proxies', 'native:recording-status', 'native:release-system-port', 'native:release-mjpeg-port', 'native:api-level', 'native:restore-policy'];
  let cases = 0;
  async function exercise({cleanupFails = false, success = false, fault = '', phase = 'original', primitive = false} = {}) {
    const trace = [], timeline = [], records = [];
    const native = value => { trace.push(value); timeline.push(value); };
    active = {native};
    const original = primitive ? 'synthetic primitive failure' : new Error('synthetic create canary; native exit=255 signal=null timedOut=false');
    const cleanup = new Error('synthetic cleanup canary; native exit=1 signal=null');
    const target = phase === 'original' ? original : cleanup;
    let faultCalls = 0;
    if (fault === 'stack') Object.defineProperty(target, 'stack', {get() { faultCalls++; throw new Error('synthetic formatter failure'); }});
    if (fault === 'coercion') {
      Object.defineProperty(target, 'stack', {value: undefined});
      Object.defineProperty(target, Symbol.toPrimitive, {value() { faultCalls++; throw new Error('synthetic coercion failure'); }});
    }
    if (typeof original === 'object') Object.freeze(original);
    Object.freeze(cleanup);
    const driver = new Driver({}, false);
    let cleanupCalls = 0;
    const result = Object.freeze({synthetic: true});
    const atFault = () => trace.at(-1) === (phase === 'original' ? 'native:create' : 'native:restore-policy');
    const logger = {debug() {}, info() {}, warn() {}};
    Object.defineProperty(logger, 'error', {get() {
      if (fault === 'method' && atFault()) { faultCalls++; throw new Error('synthetic logger method failure'); }
      return message => {
        if (fault === 'call' && atFault()) { faultCalls++; throw new Error('synthetic logger call failure'); }
        records.push(message); timeline.push(message);
      };
    }});
    Object.defineProperty(driver, 'log', {get() {
      if (fault === 'logger' && atFault()) { faultCalls++; throw new Error('synthetic logger access failure'); }
      return logger;
    }});
    driver.getDeviceInfoFromCaps = async () => { native('native:device-info'); return {udid: 'synthetic-device'}; };
    driver.createADB = async () => {
      native('native:adb');
      return {
        getApiLevel: async () => { native('native:api-level'); return 28; },
        setDefaultHiddenApiPolicy: async () => { native('native:restore-policy'); if (cleanupFails) throw cleanup; },
        stopLogcat: async () => native('native:stop-logcat'),
      };
    };
    driver.prepareSessionApp = async () => native('native:prepare');
    driver.startUiAutomator2Session = async () => { native('native:create'); if (!success) throw original; return result; };
    driver.stopChromedriverProxies = async () => native('native:stop-proxies');
    driver.mobileIsMediaProjectionRecordingRunning = async () => { native('native:recording-status'); return false; };
    driver.releaseSystemPort = async () => native('native:release-system-port');
    driver.releaseMjpegServerPort = async () => native('native:release-mjpeg-port');
    driver.deleteSession = async () => { cleanupCalls++; return Driver.prototype.deleteSession.call(driver); };
    let returned, rejected, settled = false;
    try { returned = await driver.createSession({alwaysMatch: {}, firstMatch: [{}]}); }
    catch (error) { rejected = error; settled = true; }
    assert.equal(settled, !success);
    assert.equal(cleanupCalls, success ? 0 : 1);
    if (success) {
      assert.equal(returned[0], 'synthetic-session');
      assert.equal(returned[1], result);
      assert.deepEqual(trace, createTrace);
      assert.deepEqual(records, []);
    } else {
      assert.equal(rejected, cleanupFails ? cleanup : original);
      assert.deepEqual(trace, [...createTrace, ...cleanupTrace, ...(!cleanupFails ? ['native:stop-logcat', 'base:delete'] : [])]);
    }
    if (fault) assert.equal(faultCalls, image === 'patched' ? 1 : 0);
    cases++;
    return {original, cleanup, records, timeline};
  }
  if (mode === 'behavior') {
    for (const cleanupFails of [false, true]) {
      await exercise({cleanupFails});
      await exercise({cleanupFails, primitive: true});
      for (const fault of ['logger', 'method', 'call', 'stack', 'coercion']) {
        await exercise({cleanupFails, fault});
        if (cleanupFails) await exercise({cleanupFails, fault, phase: 'cleanup'});
      }
    }
    await exercise({success: true});
  } else {
    for (const cleanupFails of [false, true]) {
      const {original, cleanup, records, timeline} = await exercise({cleanupFails});
      const originalRecord = `create-before-cleanup: ${original.stack}`;
      assert.ok(records.includes(originalRecord), retentionFailure);
      assert.ok(timeline.indexOf(originalRecord) < timeline.indexOf('claim:release'));
      assert.equal(records.length, cleanupFails ? 2 : 1);
      if (cleanupFails) {
        const cleanupRecord = `create-cleanup-failed: ${cleanup.stack}`;
        assert.ok(records.includes(cleanupRecord));
        assert.ok(timeline.indexOf(cleanupRecord) > timeline.indexOf('native:restore-policy'));
        assert.notEqual(originalRecord, cleanupRecord);
      }
    }
  }
  console.log(`PASS installed create error ${mode}: ${cases} cases`);
}
main().catch(error => {
  if (error instanceof assert.AssertionError && error.message === retentionFailure) {
    console.log('FAIL installed original-error retention assertion');
    process.exitCode = 41;
  } else {
    console.log('FAIL installed create error fixture');
    process.exitCode = 1;
  }
});
