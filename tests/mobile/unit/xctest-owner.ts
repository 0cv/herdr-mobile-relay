import { strict as assert } from 'node:assert';
import { spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import { cp, mkdtemp, mkdir, readFile, writeFile, rm, realpath, symlink } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createServer as createNetServer, type AddressInfo } from 'node:net';
import { managedWdaCapabilities } from '../support/ios-xctest';
import { IOSPlatform } from '../platforms/ios';

const shim = `#!/bin/sh
cmd=$0
cmd=\${cmd##*/}
root=$STARTUP_TEST_ROOT
mode=$STARTUP_TEST_MODE
trace=$XCTEST_COMMAND_TRACE
if [ -n "$trace" ]; then
  printf 'start %s %s' "$(date +%s%N)" "$cmd" >> "$trace"
  for argument do printf ' [%s]' "$argument" >> "$trace"; done
  printf '\\n' >> "$trace"
  trap 'printf "end %s %s\\n" "$(date +%s%N)" "$cmd" >> "$trace"' 0
fi
has_arg() {
  needle=$1
  shift
  for argument do
    [ "$argument" = "$needle" ] && return 0
  done
  return 1
}
if [ "$mode" = "delayed" ]; then
  if [ "$cmd" = "lsof" ] && has_arg -d "$@"; then
    /bin/sleep 0.6
  elif [ "$cmd" = "ps" ]; then
    delayed_ps_count=0
    [ -e "$root/delayed-ps-count" ] && delayed_ps_count=$(cat "$root/delayed-ps-count")
    printf '%s' "$((delayed_ps_count + 1))" > "$root/delayed-ps-count"
    if [ "$delayed_ps_count" -gt 0 ]; then
      /bin/sleep 0.6
    fi
  fi
fi
case "$cmd" in
plutil)
  if [ "$1" = "-extract" ]; then
    path=$6
    product_info=false
    case "$path" in
      "$STARTUP_TEST_PRODUCT/Info.plist"|/private$STARTUP_TEST_PRODUCT/Info.plist) product_info=true ;;
    esac
    if [ "$mode" = "product" ] && [ "$product_info" = true ]; then
      printf '%s\\n' wrong
    else
      bundle_mismatch=false
      case "$path" in
        "$STARTUP_TEST_RUNNER_RECEIPT"/*|/private$STARTUP_TEST_RUNNER_RECEIPT/*) bundle_mismatch=true ;;
      esac
      if [ "$mode" = "listener-bundle-mismatch" ] && [ "$bundle_mismatch" = true ]; then
        printf '%s\\n' wrong.bundle
      else
        printf '%s\\n' com.facebook.WebDriverAgentRunner.xctrunner
      fi
    fi
  elif [ "$1" = "-convert" ] && [ "$2" = "json" ]; then
    cat "$5"
  fi
  ;;
lsof)
  if [ "$mode" = "occupied" ]; then
    printf '%s\\n' 999999
    exit 0
  fi
  if [ ! -e "$root/runner.pid" ]; then
    exit 1
  fi
  if [ "$mode" = "listener-invalid-pid" ] && ! has_arg -d "$@"; then
    printf '%s\\n' 2147483648
    exit 0
  fi
  if [ "$mode" = "listener-first-failure" ] && ! has_arg -d "$@"; then
    exit 2
  fi
  if [ "$mode" = "listener-second-failure" ] && ! has_arg -d "$@" && has_arg "-iTCP:$IOS_WDA_MJPEG_PORT" "$@"; then
    exit 2
  fi
  if [ "$mode" = "listener-endpoints-disappear" ] && ! has_arg -d "$@" && [ -e "$root/status-queries" ]; then
    exit 1
  fi
  if has_arg -d "$@"; then
    requested=
    while [ "$#" -gt 0 ]; do
      if [ "$1" = "-p" ]; then
        requested=$2
        break
      fi
      shift
    done
    [ -n "$requested" ] || requested=$(cat "$root/runner.pid")
    if [ "$mode" = "listener-hash-after-freeze" ]; then
      count=0
      [ -e "$root/listener-evidence-count" ] && count=$(cat "$root/listener-evidence-count")
      printf '%s' "$((count + 1))" > "$root/listener-evidence-count"
      if [ "$count" -ge 1 ]; then
        printf '%s' 'different listener after freeze' > "$STARTUP_TEST_RUNNER_RECEIPT/WebDriverAgentRunner-Runner"
      fi
    fi
    executable=$STARTUP_TEST_RUNNER_RECEIPT/WebDriverAgentRunner-Runner
    [ "$mode" = "credential" ] && executable=$STARTUP_TEST_RUNNER_RECEIPT/unexpected-listener
    printf 'p%s\\nftxt\\nn%s\\n' "$requested" "$executable"
    exit 0
  fi
  if [ "$mode" = "listener-mjpeg-duplicate" ] && has_arg "-iTCP:$IOS_WDA_MJPEG_PORT" "$@"; then
    printf '11111\\n22222\\n'
  elif [ "$mode" = "swap" ] || [ "$mode" = "pid-reuse" ]; then
    count=0
    [ -e "$root/swap-lsof-count" ] && count=$(cat "$root/swap-lsof-count")
    printf '%s' "$((count + 1))" > "$root/swap-lsof-count"
    if [ "$mode" = "pid-reuse" ] || [ "$count" -lt 4 ]; then
      printf '%s\\n' 11111
    else
      printf '%s\\n' 22222
    fi
  else
    printf '%s\\n' "$(cat "$root/runner.pid")"
  fi
  ;;
ps)
  pid=$2
  format=$4
  xcode_pid=
  [ -e "$root/xcode.pid" ] && xcode_pid=$(cat "$root/xcode.pid")
  if { [ "$mode" = "swap" ] || [ "$mode" = "pid-reuse" ]; } && [ "$pid" != "$xcode_pid" ]; then
    generation=0
    [ -e "$root/swap-lsof-count" ] && generation=$(cat "$root/swap-lsof-count")
    reused=false
    [ "$mode" = "pid-reuse" ] && [ "$generation" -ge 5 ] && reused=true
    if [ "$format" = "lstart=" ]; then
      if [ "$reused" = true ] || [ "$pid" = 22222 ]; then
        printf '%s\\n' birth-b
      else
        printf '%s\\n' birth-a
      fi
    else
      printf 'WebDriverAgentRunner-Runner %s\\n' "$pid"
    fi
  elif [ "$format" = "comm=" ]; then
    printf '%s\\n' "$STARTUP_TEST_RECEIPT/WebDriverAgentRunner-Runner"
  elif [ "$mode" = "credential" ]; then
    printf 'xcodebuild test-without-building -xctestrun %s -destination id=%s https://example.test/?token=credential-token\\n' "$STARTUP_TEST_XCTESTRUN" "$IOS_SIMULATOR_UDID"
  else
    printf 'xcodebuild test-without-building -xctestrun %s -destination id=%s\\n' "$STARTUP_TEST_XCTESTRUN" "$IOS_SIMULATOR_UDID"
  fi
  ;;
xcrun)
  if [ "$2" = "list" ]; then
    printf '%s\\n' "$IOS_SIMULATOR_UDID"
  elif [ "$2" = "get_app_container" ]; then
    count=0
    [ -e "$root/receipt-queries" ] && count=$(cat "$root/receipt-queries")
    printf '%s' "$((count + 1))" > "$root/receipt-queries"
    if [ "$mode" = "receipt-failure" ] && [ "$count" -ge 1 ]; then
      exit 77
    elif [ "$mode" = "receipt-missing-then-valid" ] && [ "$count" -eq 1 ]; then
      printf '%s\\n' "$STARTUP_TEST_RUNNER_RECEIPT/missing-receipt"
    else
      printf '%s\\n' "$STARTUP_TEST_RECEIPT"
    fi
  elif [ "$2" = "spawn" ]; then
    printf '%s\\n' 'retained simulator diagnostic'
  fi
  ;;
xcodebuild)
  exec "$STARTUP_TEST_EXECUTABLE" "$STARTUP_TEST_WDA_SERVER" xcodebuild "$@"
  ;;
esac
exit 0
`;
const wdaServer = `import { createServer } from 'node:http';
import { appendFileSync, existsSync, readFileSync, unlinkSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
const root = process.env.STARTUP_TEST_ROOT;
const mode = process.env.STARTUP_TEST_MODE;
const args = process.argv.slice(3);
const runnerPid = join(root, 'runner.pid');
const xcodePid = join(root, 'xcode.pid');
const remove = path => { try { unlinkSync(path); } catch {} };
appendFileSync(join(root, 'launches'), JSON.stringify(args) + '\\n');
if (mode === 'early') process.exit(43);
if (mode === 'receipt-product-changed') {
  writeFileSync(join(process.env.STARTUP_TEST_PRODUCT, 'WebDriverAgentRunner-Runner'), 'changed');
  writeFileSync(join(root, 'product-changed-at'), String(Date.now()));
}
writeFileSync(xcodePid, String(process.pid));
const server = createServer((_request, response) => {
  const statusPath = join(root, 'status-queries');
  const count = existsSync(statusPath) ? Number(readFileSync(statusPath, 'utf8')) : 0;
  writeFileSync(statusPath, String(count + 1));
  const body = { value: { ready: mode !== 'invalid', state: 'success', build: { version: '16.12.1', productBundleIdentifier: 'com.facebook.WebDriverAgentRunner' }, os: { version: '18.5' } } };
  const oversized = mode === 'oversized' || (mode === 'ready-then-oversized' && count >= 1);
  response.setHeader('content-type', 'application/json');
  response.end(JSON.stringify(body) + (oversized ? ' '.repeat(70000) : ''));
});
let stopping = false;
const stop = () => {
  if (stopping) return;
  stopping = true;
  server.closeAllConnections?.();
  server.close(() => process.exit(0));
};
process.on('SIGTERM', stop);
process.on('SIGINT', stop);
process.on('exit', () => { remove(runnerPid); remove(xcodePid); });
server.listen(Number(process.env.IOS_WDA_PORT), '127.0.0.1', () => writeFileSync(runnerPid, String(process.pid)));
`;
const pause = () => new Promise(resolve => setTimeout(resolve, 25));

export const xctestOwnerTests: Array<[string, () => Promise<void>]> = [];
for (const mode of ['ready', 'ready-then-oversized', 'early', 'invalid', 'occupied', 'ownership', 'ambiguous', 'product', 'receipt-failure', 'receipt-missing-then-valid', 'receipt-product-changed', 'swap', 'pid-reuse', 'oversized', 'delayed', 'credential', 'listener-mjpeg-duplicate', 'listener-bundle-mismatch', 'listener-hash-mismatch', 'listener-invalid-pid', 'listener-first-failure', 'listener-second-failure', 'listener-endpoints-disappear', 'listener-hash-after-freeze']) {
  xctestOwnerTests.push([`Native startup actual XCTest supervisor ${mode}`, async () => {
    const root = await mkdtemp(join(tmpdir(), 'herdr-xctest-test-'));
    const udid = '82342155-D8BD-4C4D-BD5E-1EDCDF9CFB40';
    const product = join(root, 'products/Debug-iphonesimulator/WebDriverAgentRunner-Runner.app');
    const receipt = join(root, `Devices/${udid}/data/Containers/Bundle/Application/10C0E9C0-50FD-4C3E-AC55-AC1158A00568/WebDriverAgentRunner-Runner.app`);
    const runnerReceipt = join(root, `Devices/${udid}/data/Containers/Bundle/Application/1C1FB5F7-2D0E-49AC-BE09-62816F229E5C/WebDriverAgentRunner-Runner.app`);
    const state = join(root, 'state');
    const bin = join(root, 'bin');
    const xctestrun = join(root, 'products/WebDriverAgentRunner_test.xctestrun');
    await Promise.all([mkdir(bin), mkdir(state), mkdir(join(product, 'PlugIns/WebDriverAgentRunner.xctest'), { recursive: true }), mkdir(receipt, { recursive: true }), mkdir(runnerReceipt, { recursive: true }), mkdir(join(root, 'wda'))]);
    for (const dir of [product, receipt, runnerReceipt]) {
      await writeFile(join(dir, 'Info.plist'), JSON.stringify({ CFBundleIdentifier: 'com.facebook.WebDriverAgentRunner.xctrunner' }));
      await writeFile(join(dir, 'WebDriverAgentRunner-Runner'), mode === 'listener-hash-mismatch' && dir === runnerReceipt ? 'different listener' : 'exact built runner');
    }
    await writeFile(join(root, 'wda/package.json'), JSON.stringify({ version: '16.12.1' }));
    await writeFile(xctestrun, JSON.stringify({ WebDriverAgentRunner: {
      TestHostBundleIdentifier: 'com.facebook.WebDriverAgentRunner.xctrunner',
      TestBundlePath: '__TESTHOST__/PlugIns/WebDriverAgentRunner.xctest',
      TestHostPath: '__TESTROOT__/Debug-iphonesimulator/WebDriverAgentRunner-Runner.app',
    } }));
    if (mode === 'ambiguous') await writeFile(join(root, 'products/WebDriverAgentRunner_second.xctestrun'), await readFile(xctestrun));
    if (mode === 'product') await writeFile(join(product, 'Info.plist'), JSON.stringify({ CFBundleIdentifier: 'wrong' }));
    if (mode === 'credential') await writeFile(join(runnerReceipt, 'unexpected-listener'), 'unexpected listener');
    await writeFile(join(root, 'owned'), `ios:${mode === 'ownership' ? 'wrong' : udid}`);
    const dispatcher = join(bin, 'xctest-command-dispatcher');
    await writeFile(dispatcher, shim, { mode: 0o700 });
    for (const cmd of ['plutil', 'lsof', 'ps', 'xcrun', 'xcodebuild']) await symlink(dispatcher, join(bin, cmd));
    const wdaServerFile = join(bin, 'xctest-wda-server.ts');
    await writeFile(wdaServerFile, wdaServer, { mode: 0o700 });
    const reserve = createNetServer();
    await new Promise<void>((resolve, reject) => {
      reserve.once('error', reject);
      reserve.listen(0, '127.0.0.1', resolve);
    });
    const port = (reserve.address() as AddressInfo).port;
    await new Promise<void>((resolve, reject) => reserve.close(error => error ? reject(error) : resolve()));
    const env = { ...process.env, PATH: `${bin}:${process.env.PATH}`, STARTUP_TEST_ROOT: root, STARTUP_TEST_MODE: mode, STARTUP_TEST_EXECUTABLE: process.execPath, STARTUP_TEST_WDA_SERVER: wdaServerFile,
      STARTUP_TEST_RECEIPT: receipt, STARTUP_TEST_RUNNER_RECEIPT: runnerReceipt, STARTUP_TEST_PRODUCT: product, STARTUP_TEST_XCTESTRUN: xctestrun, IOS_XCTEST_STATE_DIR: state,
      IOS_SIMULATOR_UDID: udid, IOS_PLATFORM_VERSION: '18.5', IOS_WDA_PORT: String(port), IOS_WDA_MJPEG_PORT: String(port === 65535 ? port - 1 : port + 1),
      IOS_WDA_PREBUILT_PATH: product, IOS_WDA_BOOTSTRAP_PATH: join(root, 'products'), IOS_WDA_AGENT_PATH: join(root, 'wda/WebDriverAgent.xcodeproj'),
      MOBILE_DEVICE_OWNERSHIP_FILE: join(root, 'owned') };
    const initialProductHash = createHash('sha256').update(await readFile(join(product, 'WebDriverAgentRunner-Runner'))).digest('hex');
    const deadline = Date.now() + (mode === 'invalid' || mode === 'oversized' ? 2000 : mode === 'delayed' ? 1500 : 10_000);
    await writeFile(join(state, 'deadline'), String(deadline));
    const child = spawn(process.execPath, [join(import.meta.dirname, '../support/ios-xctest.ts'), 'supervise'], { env, stdio: ['ignore', 'ignore', 'pipe'] });
    const supervisorStarted = Date.now();
    let errors = '';
    child.stderr.on('data', chunk => { errors += chunk.toString(); });
    let done = false;
    let assertionsPassed = false;
    const exited = new Promise<void>(resolve => child.on('close', () => { done = true; resolve(); }));
    try {
      if (mode === 'ready' || mode === 'ready-then-oversized') {
        while (Date.now() < deadline && !done) {
          if (existsSync(join(state, 'owner.json')) && JSON.parse(await readFile(join(state, 'owner.json'), 'utf8')).ready) break;
          await pause();
        }
        const owner = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8'));
        assert.ok(owner.ready, errors || owner.error);
        const previous = { ...process.env };
        Object.assign(process.env, env);
        try {
          if (mode === 'ready-then-oversized') {
            await assert.rejects(managedWdaCapabilities(udid), /managed WDA not ready/u);
          } else {
            assert.deepEqual(await managedWdaCapabilities(udid), { 'appium:webDriverAgentUrl': `http://127.0.0.1:${port}` });
            const platform = new IOSPlatform({ deviceId: udid, origin: 'https://example.test', setupUrl: 'https://example.test/setup', outputDir: root, appiumUrl: 'http://127.0.0.1:1', certificate: '/unused' });
            let sessions = 0;
            platform.driver.create = async options => {
              sessions++;
              assert.equal(options.capabilities['appium:webDriverAgentUrl'], `http://127.0.0.1:${port}`);
              assert.equal(options.capabilities['appium:usePreinstalledWDA'], undefined);
              assert.equal(options.capabilities['appium:prebuiltWDAPath'], undefined);
              throw new Error('session request captured');
            };
            await assert.rejects(platform.startFreshDevice(), /session request captured/u);
            assert.equal(sessions, 1);
          }
        }
        finally { for (const key of Object.keys(process.env)) if (!(key in previous)) delete process.env[key]; Object.assign(process.env, previous); }
        await writeFile(join(state, 'stop'), 'test finalization');
      }
      await Promise.race([exited, new Promise((_, reject) => setTimeout(() => reject(new Error('test supervisor deadline')), 15_000))]);
      const owner = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8'));
      assert.ok(owner.endedAt);
      assert.equal(owner.ready, false);
      if (['occupied', 'ownership', 'ambiguous', 'product'].includes(mode)) assert.equal(existsSync(join(root, 'launches')), false);
      else {
        const commands = (await readFile(join(root, 'launches'), 'utf8')).trim().split('\n').map(line => JSON.parse(line));
        assert.deepEqual(commands, [['test-without-building', '-xctestrun', owner.xctestrun, '-destination', `id=${udid}`]]);
      }
      if (mode === 'early') assert.equal(owner.exitCode, 43);
      if (mode === 'invalid' || mode === 'oversized' || mode === 'delayed') assert.match(owner.error, /startup deadline/u);
      if (mode === 'oversized') assert.equal(owner.status?.truncated, true);
      if (mode === 'receipt-failure' || mode === 'receipt-missing-then-valid' || mode === 'receipt-product-changed') {
        assert.ok(owner.listenerEvidence?.[0]?.birth);
        assert.ok(owner.receiptError);
        if (mode === 'receipt-failure') {
          assert.match(owner.receiptError, /xcrun failed/u);
          assert.equal(owner.listenerValidation?.failureStage, 'validated');
          assert.equal(owner.listenerValidation?.executableHash.status, 'evaluated');
          assert.equal(owner.listenerValidation?.executableHash.expected, owner.cachedProductExecutableHash);
          assert.match(owner.cachedProductExecutableHash, /^[0-9a-f]{64}$/u);
          assert.match(owner.cachedProductExecutableHashAt, /^\d{4}-\d{2}-\d{2}T/u);
        }
        else if (mode === 'receipt-missing-then-valid') assert.match(owner.receiptError, /ENOENT|no such file/u);
        else {
          assert.match(owner.error, /registration receipt validation failed/u);
          assert.match(owner.receiptError, /registration receipt mismatch/u);
          const currentProductHash = createHash('sha256').update(await readFile(join(product, 'WebDriverAgentRunner-Runner'))).digest('hex');
          assert.notEqual(currentProductHash, initialProductHash);
          for (const installed of [receipt, runnerReceipt]) {
            assert.equal(createHash('sha256').update(await readFile(join(installed, 'WebDriverAgentRunner-Runner'))).digest('hex'), initialProductHash);
          }
          assert.equal(owner.cachedProductExecutableHash, initialProductHash);
          assert.equal(owner.listenerValidation?.cachedProductExecutableHash, initialProductHash);
          assert.equal(owner.listenerValidation?.cachedProductExecutableHashAt, owner.cachedProductExecutableHashAt);
          assert.ok(Date.parse(owner.cachedProductExecutableHashAt) >= Date.parse(owner.startedAt));
          assert.ok(Date.parse(owner.cachedProductExecutableHashAt) <= Number(await readFile(join(root, 'product-changed-at'), 'utf8')));
          assert.equal(owner.listenerValidation?.failureStage, 'validated');
          assert.deepEqual(owner.listenerValidation?.executableHash, {status: 'evaluated', expected: initialProductHash, actual: initialProductHash, matches: true});
          assert.equal(owner.installReceipt, await realpath(receipt));
          assert.equal(owner.receipt, await realpath(receipt));
          assert.equal(await readFile(join(root, 'receipt-queries'), 'utf8'), '2');
          assert.equal(owner.runnerPid, undefined);
          assert.equal(owner.runnerBirth, undefined);
          assert.equal(owner.runnerExecutable, undefined);
          assert.equal(owner.status, undefined);
        }
        assert.equal(existsSync(join(root, 'status-queries')), false);
      }
      if (mode === 'swap' || mode === 'pid-reuse') {
        assert.match(owner.error, /managed WDA listener changed/u);
        assert.equal(owner.runnerPid, '11111');
      }
      if (mode === 'delayed') assert.ok(Date.now() - supervisorStarted < 15_000);
      if (mode === 'credential') {
        const publicOwner = await readFile(join(state, 'owner.json'), 'utf8');
        const privateOwner = await readFile(join(state, 'owner-private.json'), 'utf8');
        assert.equal(publicOwner.includes('credential-token'), false);
        assert.ok(publicOwner.includes('[REDACTED]'));
        assert.ok(privateOwner.includes('credential-token'));
      }
      if (['listener-mjpeg-duplicate', 'listener-bundle-mismatch', 'listener-hash-mismatch', 'credential'].includes(mode)) {
        const validation = owner.listenerValidation;
        assert.ok(validation);
        assert.equal(validation.endpoints.wda.status, 'evaluated');
        assert.equal(validation.endpoints.wda.count, 1);
        assert.ok(validation.endpoints.wda.pids.length <= 16);
        assert.equal(validation.endpoints.mjpeg.status, 'evaluated');
        assert.ok(validation.commandStatus.every((entry: { port: number; status: number | null }) => Number.isInteger(entry.port)));
        assert.equal(validation.runnerEvidencePresent, 'present');
        assert.equal(validation.runnerAssociation, 'not-evaluated');
        assert.equal(validation.mjpegAssociation, mode === 'listener-mjpeg-duplicate' ? 'mismatch' : 'not-evaluated');
        if (mode === 'listener-mjpeg-duplicate') {
          assert.equal(validation.endpoints.mjpeg.count, 2);
          assert.deepEqual(validation.endpoints.mjpeg.pids, ['11111', '22222']);
          assert.equal(validation.failureStage, 'mjpeg-pid-count');
          assert.equal(validation.errorCategory, 'listener-endpoint-cardinality');
        }
        if (mode === 'listener-bundle-mismatch') {
          assert.equal(validation.bundleId.status, 'evaluated');
          assert.equal(validation.bundleId.matches, false);
          assert.equal(validation.executableHash.status, 'not-evaluated');
          assert.equal(validation.failureStage, 'runner-bundle-id');
        }
        if (mode === 'listener-hash-mismatch') {
          assert.equal(validation.bundleId.matches, true);
          assert.equal(validation.executableHash.status, 'evaluated');
          assert.equal(validation.executableHash.matches, false);
          assert.match(validation.executableHash.expected, /^[0-9a-f]{64}$/u);
          assert.match(validation.executableHash.actual, /^[0-9a-f]{64}$/u);
          assert.equal(validation.failureStage, 'runner-executable-hash');
        }
        if (mode === 'credential') {
          assert.equal(validation.pathShape.executableName, false);
          assert.equal(validation.bundleId.status, 'not-evaluated');
          assert.equal(validation.executableHash.status, 'not-evaluated');
          assert.equal(validation.failureStage, 'runner-executable-name');
        }
      }
      if (['listener-invalid-pid', 'listener-first-failure', 'listener-second-failure', 'listener-endpoints-disappear', 'listener-hash-after-freeze'].includes(mode)) {
        const validation = owner.listenerValidation;
        assert.ok(validation);
        assert.ok(validation.commandStatus.every((entry: { port: number; status: number | null }) => Number.isInteger(entry.port)));
        if (mode === 'listener-invalid-pid') {
          assert.deepEqual(validation.endpoints.wda, {status: 'error', errorCategory: 'invalid-process-id'});
          assert.deepEqual(validation.endpoints.mjpeg, {status: 'not-evaluated'});
          assert.equal(validation.commandStatus[0].status, 0);
          assert.equal(validation.failureStage, 'wda-listener-command');
          assert.equal(validation.errorCategory, 'listener-invalid-process-id');
        }
        if (mode === 'listener-first-failure') {
          assert.deepEqual(validation.endpoints.wda, {status: 'error', errorCategory: 'command-error'});
          assert.deepEqual(validation.endpoints.mjpeg, {status: 'not-evaluated'});
          assert.equal(validation.runnerEvidencePresent, 'not-evaluated');
          assert.equal(validation.commandStatus[0].status, 2);
          assert.equal(validation.failureStage, 'wda-listener-command');
        }
        if (mode === 'listener-second-failure') {
          assert.deepEqual(validation.endpoints.wda, {status: 'evaluated', count: 1, pids: [String(owner.pid)]});
          assert.deepEqual(validation.endpoints.mjpeg, {status: 'error', errorCategory: 'command-error'});
          assert.equal(validation.runnerEvidencePresent, 'not-evaluated');
          assert.equal(validation.commandStatus.at(-1).status, 2);
          assert.equal(validation.failureStage, 'mjpeg-listener-command');
        }
        if (mode === 'listener-endpoints-disappear') {
          assert.deepEqual(validation.endpoints.wda, {status: 'evaluated', count: 0, pids: []});
          assert.deepEqual(validation.endpoints.mjpeg, {status: 'evaluated', count: 0, pids: []});
          assert.equal(validation.runnerEvidencePresent, 'not-evaluated');
          assert.equal(validation.runnerAssociation, 'mismatch');
          assert.equal(validation.mjpegAssociation, 'not-evaluated');
          assert.equal(validation.failureStage, 'managed-wda-pid-count');
        }
        if (mode === 'listener-hash-after-freeze') {
          assert.equal(validation.endpoints.wda.status, 'evaluated');
          assert.equal(validation.runnerEvidencePresent, 'present');
          assert.equal(validation.runnerAssociation, 'match');
          assert.equal(validation.mjpegAssociation, 'match');
          assert.equal(validation.executableHash.status, 'evaluated');
          assert.equal(validation.executableHash.matches, false);
          assert.equal(validation.executableHash.expected, owner.cachedProductExecutableHash);
          assert.notEqual(validation.executableHash.actual, validation.executableHash.expected);
          assert.equal(validation.failureStage, 'runner-executable-hash');
        }
      }
      if (mode === 'ready') assert.equal(owner.error, undefined);
      if (mode !== 'ownership') assert.match(await readFile(join(state, 'ios-wda-system.log'), 'utf8'), /retained simulator diagnostic/u);
      assert.equal(existsSync(join(root, 'runner.pid')), false);
      assertionsPassed = true;
    } finally {
      if (!done) { await writeFile(join(state, 'stop'), 'failed test cleanup'); child.kill('SIGTERM'); await exited; }
      const diagnosticRoot = process.env.XCTEST_DIAGNOSTIC_ROOT;
      if (!assertionsPassed && diagnosticRoot) {
        await rm(diagnosticRoot, { recursive: true, force: true });
        await cp(root, diagnosticRoot, { recursive: true, force: true });
      }
      await rm(root, { recursive: true, force: true });
    }
  }]);
}

const runStopCli = (env: NodeJS.ProcessEnv): Promise<{ code: number | null; stderr: string }> => new Promise(resolve => {
  const child = spawn(process.execPath, [join(import.meta.dirname, '../support/ios-xctest.ts'), 'stop'], { env, stdio: ['ignore', 'ignore', 'pipe'] });
  let stderr = '';
  child.stderr.on('data', chunk => { stderr += chunk.toString(); });
  child.on('close', (code) => resolve({ code, stderr }));
});

xctestOwnerTests.push(['Native startup stop CLI propagates recorded cleanup errors', async () => {
  const root = await mkdtemp(join(tmpdir(), 'herdr-xctest-stop-'));
  const state = join(root, 'state');
  const udid = '82342155-D8BD-4C4D-BD5E-1EDCDF9CFB40';
  await mkdir(state);
  await writeFile(join(root, 'owned'), `ios:${udid}`);
  const owner: { udid: string; product: string; url: string; startedAt: string; ready: boolean; endedAt: string; error?: string } = { udid, product: '', url: 'http://127.0.0.1:8100', startedAt: new Date().toISOString(), ready: false,
    endedAt: new Date().toISOString(), error: 'XCTEST: cleanup failed https://example.test/?token=stop-secret' };
  const privateSerialized = `${JSON.stringify(owner)}\n`;
  const publicSerialized = `${JSON.stringify({ ...owner, error: 'XCTEST: cleanup failed https://example.test/?token=[REDACTED]' })}\n`;
  await writeFile(join(state, 'owner-private.json'), privateSerialized, { mode: 0o600 });
  await writeFile(join(state, 'owner.json'), publicSerialized, { mode: 0o600 });
  const env = { ...process.env, IOS_XCTEST_STATE_DIR: state, IOS_SIMULATOR_UDID: udid, MOBILE_DEVICE_OWNERSHIP_FILE: join(root, 'owned') };
  try {
    const failed = await runStopCli(env);
    assert.notEqual(failed.code, 0);
    assert.match(failed.stderr, /cleanup failed/u);
    assert.equal(failed.stderr.includes('stop-secret'), false);
    assert.ok(failed.stderr.includes('[REDACTED]'));
    owner.error = undefined;
    await writeFile(join(state, 'owner-private.json'), `${JSON.stringify(owner)}\n`, { mode: 0o600 });
    await writeFile(join(state, 'owner.json'), `${JSON.stringify(owner)}\n`, { mode: 0o600 });
    const clean = await runStopCli(env);
    assert.equal(clean.code, 0, clean.stderr);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
}]);
