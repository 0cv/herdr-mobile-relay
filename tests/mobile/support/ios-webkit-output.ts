import { open } from 'node:fs/promises';
import { StringDecoder } from 'node:string_decoder';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';
import { createRequire } from 'node:module';
import { writeSanitizedJson } from './diagnostics';

const maximumInputBytes = 8 * 1024 * 1024;
const maximumLineBytes = 4_096;
const maximumEvents = 256;

type WebKitStage = 'contexts-start' | 'views-start' | 'select-app-start' | 'app-attempt'
  | 'listing-send' | 'rpc-response' | 'app-selected' | 'select-app-end' | 'contexts-ready'
  | 'contexts-empty' | 'no-pages' | 'page-search-error' | 'app-not-connectable'
  | 'socket-connected' | 'socket-disconnected' | 'debugger-version' | 'installed-candidate';

interface WebKitEvent {
  ordinal: number;
  timestampMs: number;
  stage: WebKitStage;
  appId?: number;
  messageId?: number;
  durationMs?: number;
  hostId?: number;
  nativePid?: number;
  role?: 'webapp' | 'service' | 'generic' | 'other' | 'unknown';
  reason?: 'inactive' | 'unreported' | 'malformed' | 'unsupported' | 'wrong-host' | 'proxy' | 'unowned' | 'provider' | 'host';
  action?: 'list' | 'skip';
}

export class IOSWebKitOutputDiagnostics {
  readonly data = {
    schema: 1,
    source: 'appium-log-allowlist' as const,
    diagnosticOnly: true,
    inputTruncated: false,
    completeRecords: 0,
    recognizedRecords: 0,
    unknownRecords: 0,
    oversizedRecords: 0,
    incompleteRecords: 0,
    droppedEvents: 0,
    remoteDebuggerVersion: undefined as string | undefined,
    events: [] as WebKitEvent[],
  };
  private pending = '';
  private bytes = 0;
  private oversized = false;
  private finalized = false;

  constructor(truncated = false) {
    this.data.inputTruncated = truncated;
    // The tail can start inside a payload; never interpret its first partial line.
    this.oversized = truncated;
  }

  observe(text: string): void {
    if (this.finalized) return;
    let offset = 0;
    while (offset < text.length) {
      const newline = text.indexOf('\n', offset);
      const end = newline === -1 ? text.length : newline;
      if (!this.oversized) {
        const part = text.slice(offset, end);
        this.bytes += Buffer.byteLength(part);
        if (this.bytes > maximumLineBytes) {
          this.pending = '';
          this.oversized = true;
          this.data.oversizedRecords++;
        } else this.pending += part;
      }
      if (newline === -1) return;
      this.data.completeRecords++;
      if (!this.oversized) this.record(this.pending);
      this.pending = '';
      this.bytes = 0;
      this.oversized = false;
      offset = newline + 1;
    }
  }

  finish(): void {
    if (this.finalized) return;
    if (this.bytes > 0 || this.oversized) this.data.incompleteRecords++;
    this.pending = '';
    this.bytes = 0;
    this.oversized = false;
    this.finalized = true;
  }

  private record(raw: string): void {
    const line = raw.replace(new RegExp(`${String.fromCharCode(27)}\\[[0-9;]*m`, 'gu'), '');
    // Appium 3.7's console sink joins session/component brackets and adds " - " after its UTC timestamp.
    const frame = /^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}:\d{3}) - (?:\[[0-9a-f]{8}\])?\[(RemoteDebugger|XCUITestDriver)(?:@[0-9a-f]{4})?\] ([^\r\n]+)\r?$/u.exec(line);
    const timestampMs = frame ? Date.parse(`${frame[1].replace(' ', 'T').replace(/:(\d{3})$/u, '.$1')}Z`) : NaN;
    if (!frame || !Number.isFinite(timestampMs) || line.includes('\uFFFD')) {
      this.data.unknownRecords++;
      return;
    }
    const [, , component, message] = frame;
    const event = { timestampMs } as Omit<WebKitEvent, 'ordinal'>;
    const match = (pattern: RegExp): RegExpExecArray | null => pattern.exec(message);
    let fields: RegExpExecArray | null;
    if (component === 'XCUITestDriver') {
      if (message === 'Getting list of available contexts') event.stage = 'contexts-start';
      else if (message === 'Retrieving contexts and views') event.stage = 'views-start';
      else if ((fields = match(/^Found webview context after (\d{1,9})ms$/u))) {
        event.stage = 'contexts-ready'; event.durationMs = Number(fields[1]);
      } else if ((fields = match(/^No webviews found in (\d{1,9})ms$/u))) {
        event.stage = 'contexts-empty'; event.durationMs = Number(fields[1]);
      } else if (match(/^No web frames found after \d{1,3} retr(?:y|ies)$/u)) event.stage = 'no-pages';
      else if (match(/^No available web pages after \d{1,3} retr(?:y|ies): /u)) event.stage = 'page-search-error';
    } else {
      if ((fields = match(/^Installed candidate app=(\d{1,10}) host=(\d{1,10}) native=(\d{1,10}) role=(webapp|service|generic|other|unknown) reason=(inactive|unreported|malformed|unsupported|wrong-host|proxy|unowned|provider|host) action=(list|skip)$/u))) {
        event.stage = 'installed-candidate'; event.appId = Number(fields[1]);
        event.hostId = Number(fields[2]); event.nativePid = Number(fields[3]);
        event.role = fields[4] as WebKitEvent['role']; event.reason = fields[5] as WebKitEvent['reason'];
        event.action = fields[6] as WebKitEvent['action'];
      } else if (message === 'Selecting application') event.stage = 'select-app-start';
      else if (message === 'Debugger socket connected') event.stage = 'socket-connected';
      else if (message === 'Debugger socket disconnected') event.stage = 'socket-disconnected';
      else if ((fields = match(/^Attempting app 'PID:(\d{1,10})'$/u))) {
        event.stage = 'app-attempt'; event.appId = Number(fields[1]);
      } else if ((fields = match(/^Sending '_rpc_forwardGetListing:' message to app 'PID:(\d{1,10})' \(id: (\d{1,10})\): 'connectToApp'$/u))) {
        event.stage = 'listing-send'; event.appId = Number(fields[1]); event.messageId = Number(fields[2]);
      } else if ((fields = match(/^Received (?:data )?response from send \(id: (\d{1,10})\): '/u))) {
        event.stage = 'rpc-response'; event.messageId = Number(fields[1]);
      } else if ((fields = match(/^Finally selecting app PID:(\d{1,10})$/u))) {
        event.stage = 'app-selected'; event.appId = Number(fields[1]);
      } else if ((fields = match(/^Selected app after (\d{1,9})ms$/u))) {
        event.stage = 'select-app-end'; event.durationMs = Number(fields[1]);
      } else if ((fields = match(/^The application PID:(\d{1,10}) is not connectable yet: /u))) {
        event.stage = 'app-not-connectable'; event.appId = Number(fields[1]);
      } else if ((fields = match(/^Remote Debugger version (\d{1,3}\.\d{1,3}\.\d{1,3})$/u))) {
        event.stage = 'debugger-version'; this.data.remoteDebuggerVersion = fields[1];
      }
    }
    if (!event.stage) { this.data.unknownRecords++; return; }
    this.data.events.push({ ordinal: ++this.data.recognizedRecords, ...event });
    if (this.data.events.length > maximumEvents) {
      this.data.events.shift();
      this.data.droppedEvents++;
    }
  }
}

type PackageVersion = { status: 'available'; version: string } | { status: 'unavailable' };

async function packageVersion(path: string): Promise<PackageVersion> {
  const handle = await open(path, 'r');
  try {
    const buffer = Buffer.alloc(16_385);
    let length = 0;
    while (length < buffer.length) {
      const { bytesRead } = await handle.read(buffer, length, buffer.length - length, length);
      if (!bytesRead) break;
      length += bytesRead;
    }
    if (length === buffer.length) return { status: 'unavailable' };
    const { version } = JSON.parse(buffer.subarray(0, length).toString('utf8'));
    return typeof version === 'string' && /^\d{1,3}\.\d{1,3}\.\d{1,3}$/u.test(version)
      ? { status: 'available', version } : { status: 'unavailable' };
  } finally { await handle.close(); }
}

async function runtimeVersions(appiumHome?: string): Promise<{ xcuitest: PackageVersion; remoteDebugger: PackageVersion }> {
  const versions = { xcuitest: { status: 'unavailable' } as PackageVersion, remoteDebugger: { status: 'unavailable' } as PackageVersion };
  if (!appiumHome) return versions;
  try {
    const driverManifest = createRequire(resolve(appiumHome, 'package.json')).resolve('appium-xcuitest-driver/package.json');
    versions.xcuitest = await packageVersion(driverManifest);
    const remoteManifest = createRequire(driverManifest).resolve('appium-remote-debugger/package.json');
    versions.remoteDebugger = await packageVersion(remoteManifest);
  } catch { /* Missing/invalid manifests remain explicit unavailable observations, never raw errors. */ }
  return versions;
}

export async function collectIOSWebKitOutput(input: string, output: string, appiumHome?: string): Promise<void> {
  const versions = await runtimeVersions(appiumHome);
  let handle;
  try { handle = await open(input, 'r'); }
  catch (error) {
    if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw new Error('IOS_WEBKIT_DIAGNOSTICS: cannot open private log', { cause: error });
    await writeSanitizedJson(output, { ...new IOSWebKitOutputDiagnostics().data, runtimeVersions: versions, status: 'missing', sourceBytes: 0, scannedBytes: 0 });
    return;
  }
  try {
    const { size } = await handle.stat();
    if (!Number.isSafeInteger(size) || size < 0) throw new Error('IOS_WEBKIT_DIAGNOSTICS: invalid log size');
    const start = Math.max(0, size - maximumInputBytes);
    const diagnostics = new IOSWebKitOutputDiagnostics(start > 0);
    const decoder = new StringDecoder('utf8');
    const buffer = Buffer.alloc(65_536);
    let position = start;
    while (position < size) {
      const { bytesRead } = await handle.read(buffer, 0, Math.min(buffer.length, size - position), position);
      if (!bytesRead) break;
      diagnostics.observe(decoder.write(buffer.subarray(0, bytesRead)));
      position += bytesRead;
    }
    diagnostics.observe(decoder.end());
    diagnostics.finish();
    await writeSanitizedJson(output, { ...diagnostics.data, runtimeVersions: versions, status: position === size ? 'captured' : 'short-read', sourceBytes: size, scannedBytes: position - start });
  } finally { await handle.close(); }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const [input, output] = process.argv.slice(2);
  if (!input || !output || process.argv.length !== 4) throw new Error('IOS_WEBKIT_DIAGNOSTICS: expected private-log output-json');
  await collectIOSWebKitOutput(input, output, process.env.APPIUM_HOME);
}
