import { appendFileSync, existsSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

const args = process.argv.slice(2);
const directory = process.env.FAKE_ANDROID_FIXTURE_DIR!;
const state = process.env.FAKE_ANDROID_STATE || 'valid';
appendFileSync(process.env.FAKE_ANDROID_LOG!, args.join(' ') + '\n');
const filename = (name: string) => join(directory, `${state}-${name}`);
const read = (name: string) => readFileSync(filename(name), 'utf8');
const vendingFile = filename('vending.json');
const vending = existsSync(vendingFile) ? JSON.parse(readFileSync(vendingFile, 'utf8')) : { absent: true };
const processPackage = vending.foregroundPackage || 'com.android.chrome';
const processPid = vending.foregroundPackage ? '6600' : '6538';
const children: Record<string, string> = vending.children || {};
const output = (value: string) => { process.stdout.write(value); };
const fail = (message = '') => { process.stderr.write(message); process.exit(1); };
const sleep = async () => { await new Promise((resolve) => setTimeout(resolve, 2000)); };
const dump = () => `Packages:\n  Package [com.android.vending] (fixture):\n    codePath=${vending.path || '/product/priv-app/Phonesky'}\n    versionCode=${vending.version || '123'} minSdk=23 targetSdk=35\n    versionName=1.0\n    flags=[ SYSTEM HAS_CODE ]\n    splits=[base]\n    installerPackageName=null\n    initiatingPackageName=null\n    originatingPackageName=null\n    packageSource=0\n    lastUpdateTime=2026-01-01 00:00:00\n    User ${vending.user ?? 0}: installed=${vending.installed ?? true} hidden=false suspended=false stopped=true enabled=${vending.enabled ?? 0}\n      firstInstallTime=2026-01-01 00:00:00\n${vending.components ? '      disabledComponents:\n        com.android.vending.SomeComponent\n' : ''}Queries:\nCompiler stats:\n  [com.android.vending]\n    (No recorded stats)\n`;
const request = args.slice(2).join(' ');
const nativeLog = join(directory, 'native.log');
const stoppedFile = join(directory, 'chrome-stopped');
const nativeLine = (tag: string, message: string) => appendFileSync(nativeLog, `${(Date.now() / 1000).toFixed(3)} 546 1761 I ${tag}: ${message}\n`);
if (args.join(' ') === 'version') output('Android Debug Bridge version 1.0.41\nVersion 35.0.2-12147458\nInstalled as /hypothetical/adb\n');
else if (request === 'emu avd name') output(`${vending.avd || 'herdr-mobile-ci-fixture'}\nOK\n`);
else if (args[0] !== '-s' || args[1] !== 'emulator-5554') fail('unexpected serial');
else if (request === 'shell getprop') output(readFileSync(join(directory, 'getprop'), 'utf8'));
else if (request === 'shell am get-current-user') output(`${vending.foregroundUser ?? 0}\n`);
else if (request === 'shell dumpsys activity activities') output(`ResumedActivity: ActivityRecord{fixture u0 ${processPackage}/org.chromium.chrome.browser.webapps.WebappActivity t1 pid=${processPid}}\n`);
else if (request === 'shell input keyevent KEYCODE_HOME') output('');
else if (request === 'shell ps -A -o PID,NAME') {
  if (vending.processListFail) fail('process inventory denied\n');
  const visibleChildren = existsSync(stoppedFile) ? vending.remainingChildren || {} : children;
  output(vending.processList ?? `PID NAME\n${existsSync(stoppedFile) && processPid === '6538' ? '' : '6538 com.android.chrome\n'}${processPid === '6600' && !existsSync(stoppedFile) ? `6600 ${processPackage}\n` : ''}${Object.entries(visibleChildren).map(([pid, name]) => `${pid} ${name}\n`).join('')}1427 com.google.android.gms\n1486 com.google.android.googlequicksearchbox:search\n`);
}
else if (request === 'logcat -b main -b system -v epoch -T 1') {
  if (vending.logcatFail) fail('logcat: Unexpected EOF!\n');
  let offset = 0;
  setInterval(() => {
    if (!existsSync(nativeLog)) return;
    const content = readFileSync(nativeLog, 'utf8');
    output(content.slice(offset));
    offset = content.length;
  }, 10);
} else if (request.startsWith('shell log -p i -t HerdrMeasure ')) nativeLine('HerdrMeasure', args[8].slice(1, -1));
else if (request === `shell pidof ${processPackage}`) {
  if (existsSync(stoppedFile)) fail();
  output(`${processPid}\n`);
} else if (request === `shell am force-stop --user 0 ${processPackage}`) {
  if (vending.forceStopFail) fail('force-stop denied\n');
  for (const [pid, name] of Object.entries({ [processPid]: processPackage, ...children, ...vending.unobservedChildren })) {
    if (vending.omitKillingPid !== pid) nativeLine('ActivityManager', `Killing ${pid}:${name}/u0a146 (adj 0): ${vending.killReason || `stop ${processPackage} due to from pid 2000`}`);
    nativeLine('ActivityManager', `Process ${name} (pid ${pid}) has died: fg TOP`);
    nativeLine('Process', `Sending signal. PID: ${pid} SIG: 9`);
  }
  writeFileSync(stoppedFile, 'true');
}
else if (request === 'shell pm disable-user --user 0 com.android.vending') {
  if ([2, 4].includes(vending.enabled)) fail('java.lang.SecurityException: Shell cannot change component state for null to 3\n');
  if (vending.disableFail) fail('Failure [disable denied]');
  if (!vending.readbackFail) vending.enabled = 3;
  writeFileSync(vendingFile, JSON.stringify(vending));
  output(vending.disableOutput ?? 'Package com.android.vending new state: disabled-user\n');
} else if (request === 'shell dumpsys package com.android.vending') {
  output(vending.dump ?? (vending.absent ? 'Unable to find package: com.android.vending\n' : dump()));
} else if (request === 'shell pm path --user 0 com.android.vending') {
  if (vending.installed === false || vending.absent) fail();
  output(`package:${vending.path || '/product/priv-app/Phonesky'}/Phonesky.apk\n`);
}
else if (/^shell pm list packages (?:(?:-d|-e) )?--user 0 com.android.vending$/u.test(request) || request === 'shell pm list packages com.android.vending') {
  if (vending.list !== undefined) output(vending.list);
  else {
    const installed = !vending.absent && vending.installed !== false;
    const disabled = [2, 3, 4].includes(vending.enabled ?? 0);
    const match = request.includes(' -d ') ? disabled : request.includes(' -e ') ? !disabled : true;
    output(installed && match ? 'package:com.android.vending\n' : '');
  }
} else if (request === 'shell pm list packages --match-libraries -f --show-versioncode --user 0 com.google.android.trichromelibrary') {
  if (state === 'listing-timeout') await sleep();
  if (state === 'listing-failure') fail('Error: Unknown option: --match-libraries https://fixture.test/?token=android-fixture-secret\n');
  output(read('trichrome.list'));
} else if (args.slice(2, 5).join(' ') === 'shell test -f') {
  if (state === 'missing-file') fail();
  if (state === 'file-timeout') await sleep();
  if (args.length !== 6 || args[5] !== read('trichrome.file')) fail();
} else {
  const packages: Record<string, string> = { 'com.android.chrome': 'chrome', 'com.google.android.gms': 'gms', 'com.google.android.trichromelibrary_677820038': 'trichrome' };
  const name = packages[args[5]];
  if (args.slice(2, 5).join(' ') === 'shell dumpsys package' && name) {
    if (name === 'chrome' && state === 'adb-timeout') await sleep();
    if (name === 'trichrome' && state === 'adb-failure') fail('Failure [static package unavailable]\n');
    output(read(`${name}.dump`));
  } else if (args.slice(2, 5).join(' ') === 'shell pm path' && name && name !== 'trichrome') output(read(`${name}.path`));
  else fail();
}
