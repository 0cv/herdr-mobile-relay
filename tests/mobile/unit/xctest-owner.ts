import { strict as assert } from 'node:assert';
import { spawn } from 'node:child_process';
import { mkdtemp, mkdir, readFile, writeFile, rm } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createServer, type AddressInfo } from 'node:net';
import { managedWdaCapabilities } from '../support/ios-xctest';
import { IOSPlatform } from '../platforms/ios';

const shim = `#!/usr/bin/env bun
import {readFileSync,writeFileSync,appendFileSync,existsSync,unlinkSync} from 'node:fs';
import {basename,join} from 'node:path';
const root=process.env.STARTUP_TEST_ROOT;
const cmd=basename(process.argv[1]);
const args=process.argv.slice(2);
const mode=process.env.STARTUP_TEST_MODE;
const pidfile=join(root,'runner.pid');
const xcodePidfile=join(root,'xcode.pid');
const delayed=()=>{
  if(mode!=='delayed') return;
  if(cmd==='lsof'&&args.includes('-d')) { const end=Date.now()+600; while(Date.now()<end){}; return; }
  if(cmd==='ps') {
    const path=join(root,'delayed-ps-count');
    const count=existsSync(path)?Number(readFileSync(path,'utf8')):0;
    writeFileSync(path,String(count+1));
    if(count===0) return;
    const end=Date.now()+600;
    while(Date.now()<end){}
  }
};
delayed();
if(cmd==='plutil') {
  if(args[0]==='-extract') console.log(JSON.parse(readFileSync(args.at(-1),'utf8')).CFBundleIdentifier);
  else if(args[1]==='json') console.log(readFileSync(args.at(-1),'utf8'));
} else if(cmd==='lsof') {
  if(mode==='occupied') console.log('999999');
  else if(existsSync(pidfile) && args.includes('-d')) {
    const requested=args[args.indexOf('-p')+1]||readFileSync(pidfile,'utf8');
    const executable=mode==='credential'?process.env.STARTUP_TEST_RUNNER_RECEIPT+'/unexpected-listener':process.env.STARTUP_TEST_RUNNER_RECEIPT+'/WebDriverAgentRunner-Runner';
    console.log('p'+requested+'\\nftxt\\nn'+executable);
  } else if(existsSync(pidfile)) {
    if(mode==='swap'||mode==='pid-reuse') {
      const path=join(root,'swap-lsof-count');
      const count=existsSync(path)?Number(readFileSync(path,'utf8')):0;
      writeFileSync(path,String(count+1));
      console.log(mode==='pid-reuse'?'11111':count<4?'11111':'22222');
    } else console.log(readFileSync(pidfile,'utf8'));
  } else process.exit(1);
} else if(cmd==='ps') {
  const pid=args[1];
  const xcodePid=existsSync(xcodePidfile)?readFileSync(xcodePidfile,'utf8'):'';
  if((mode==='swap'||mode==='pid-reuse')&&pid!==xcodePid) {
    const generation=existsSync(join(root,'swap-lsof-count'))?Number(readFileSync(join(root,'swap-lsof-count'),'utf8')):0;
    const reused=mode==='pid-reuse'&&generation>=5;
    console.log(args.at(-1)==='lstart='?(reused||pid==='22222'?'birth-b':'birth-a'):'WebDriverAgentRunner-Runner '+pid);
  } else if(args.at(-1)==='comm=') console.log(process.env.STARTUP_TEST_RECEIPT+'/WebDriverAgentRunner-Runner');
  else if(mode==='credential') console.log('xcodebuild test-without-building -xctestrun '+process.env.STARTUP_TEST_XCTESTRUN+' -destination id='+process.env.IOS_SIMULATOR_UDID+' https://example.test/?token=credential-token');
  else console.log('xcodebuild test-without-building -xctestrun '+process.env.STARTUP_TEST_XCTESTRUN+' -destination id='+process.env.IOS_SIMULATOR_UDID);
} else if(cmd==='xcrun') {
  if(args[1]==='list') console.log(process.env.IOS_SIMULATOR_UDID);
  else if(args[1]==='get_app_container') {
    const path=join(root,'receipt-queries');
    const count=existsSync(path)?Number(readFileSync(path,'utf8')):0;
    writeFileSync(path,String(count+1));
    if(mode==='receipt-failure'&&count>=1) process.exit(77);
    if(mode==='receipt-missing-then-valid'&&count===1) console.log(process.env.STARTUP_TEST_RUNNER_RECEIPT+'/missing-receipt');
    else console.log(process.env.STARTUP_TEST_RECEIPT);
  }
  else if(args[1]==='spawn') console.log('retained simulator diagnostic');
} else if(cmd==='xcodebuild') {
  appendFileSync(join(root,'launches'),JSON.stringify(args)+'\\n');
  if(mode==='early') process.exit(43);
  writeFileSync(pidfile,String(process.pid));
  writeFileSync(xcodePidfile,String(process.pid));
  const server=Bun.serve({hostname:'127.0.0.1',port:Number(process.env.IOS_WDA_PORT),fetch(){
    const statusPath=join(root,'status-queries');
    const count=existsSync(statusPath)?Number(readFileSync(statusPath,'utf8')):0;
    writeFileSync(statusPath,String(count+1));
    const body={value:{ready:mode!=='invalid',state:'success',build:{version:'16.12.1',productBundleIdentifier:'com.facebook.WebDriverAgentRunner'},os:{version:'18.5'}}};
    const oversized=mode==='oversized'||(mode==='ready-then-oversized'&&count>=1);
    return oversized?new Response(JSON.stringify(body)+' '.repeat(70000),{headers:{'content-type':'application/json'}}):Response.json(body);
  }});
  process.on('SIGTERM',()=>{unlinkSync(pidfile);unlinkSync(xcodePidfile);server.stop(true);process.exit(0)});
}
`;
const pause = () => new Promise(resolve => setTimeout(resolve, 25));

export const xctestOwnerTests: Array<[string, () => Promise<void>]> = [];
for (const mode of ['ready', 'ready-then-oversized', 'early', 'invalid', 'occupied', 'ownership', 'ambiguous', 'product', 'receipt-failure', 'receipt-missing-then-valid', 'swap', 'pid-reuse', 'oversized', 'delayed', 'credential']) {
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
      await writeFile(join(dir, 'WebDriverAgentRunner-Runner'), 'exact built runner');
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
    for (const cmd of ['plutil', 'lsof', 'ps', 'xcrun', 'xcodebuild']) await writeFile(join(bin, cmd), shim, { mode: 0o700 });
    const reserve = createServer();
    await new Promise<void>(resolve => reserve.listen(0, '127.0.0.1', resolve));
    const port = (reserve.address() as AddressInfo).port;
    await new Promise<void>((resolve, reject) => reserve.close(error => error ? reject(error) : resolve()));
    const env = { ...process.env, PATH: `${bin}:${process.env.PATH}`, STARTUP_TEST_ROOT: root, STARTUP_TEST_MODE: mode,
      STARTUP_TEST_RECEIPT: receipt, STARTUP_TEST_RUNNER_RECEIPT: runnerReceipt, STARTUP_TEST_XCTESTRUN: xctestrun, IOS_XCTEST_STATE_DIR: state,
      IOS_SIMULATOR_UDID: udid, IOS_PLATFORM_VERSION: '18.5', IOS_WDA_PORT: String(port), IOS_WDA_MJPEG_PORT: String(port === 65535 ? port - 1 : port + 1),
      IOS_WDA_PREBUILT_PATH: product, IOS_WDA_BOOTSTRAP_PATH: join(root, 'products'), IOS_WDA_AGENT_PATH: join(root, 'wda/WebDriverAgent.xcodeproj'),
      MOBILE_DEVICE_OWNERSHIP_FILE: join(root, 'owned') };
    const deadline = Date.now() + (mode === 'invalid' || mode === 'oversized' ? 2000 : mode === 'delayed' ? 1500 : 10_000);
    await writeFile(join(state, 'deadline'), String(deadline));
    const child = spawn(process.execPath, [join(import.meta.dirname, '../support/ios-xctest.ts'), 'supervise'], { env, stdio: ['ignore', 'ignore', 'pipe'] });
    const supervisorStarted = Date.now();
    let errors = '';
    child.stderr.on('data', chunk => { errors += chunk.toString(); });
    let done = false;
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
      if (mode === 'receipt-failure' || mode === 'receipt-missing-then-valid') {
        assert.ok(owner.listenerEvidence?.[0]?.birth);
        assert.ok(owner.receiptError);
        if (mode === 'receipt-failure') assert.match(owner.receiptError, /xcrun failed/u);
        else assert.match(owner.receiptError, /ENOENT|no such file/u);
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
      if (mode === 'ready') assert.equal(owner.error, undefined);
      if (mode !== 'ownership') assert.match(await readFile(join(state, 'ios-wda-system.log'), 'utf8'), /retained simulator diagnostic/u);
      assert.equal(existsSync(join(root, 'runner.pid')), false);
    } finally {
      if (!done) { await writeFile(join(state, 'stop'), 'failed test cleanup'); child.kill('SIGTERM'); await exited; }
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
