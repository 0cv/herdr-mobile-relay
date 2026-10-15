import { createHash } from "node:crypto";
import { constants, openSync, closeSync, fstatSync, lstatSync, readSync, readdirSync, writeSync, fsyncSync, unlinkSync } from "node:fs";
import { dirname, join, resolve, sep } from "node:path";

const fileLimit = 512*1024;
const rootLimit = 2*1024*1024;
const sha256 = bytes => createHash("sha256").update(bytes).digest("hex");
const errorText = error => {
  try { return String(error).slice(0, 2048); }
  catch { return "error message unavailable"; }
};
const envelope = (kind, payload, complete = true) => ({ schema_version: 1, kind, source_kind: "real", complete, truncated: false, ambiguous: false, skipped: false, ...payload });

function privatePath(path, directory, uid, mode) {
  if (resolve(path) !== path) throw new Error("noncanonical Pi evidence path");
  let current = sep;
  for (const component of path.split(sep).filter(Boolean)) {
    current = join(current, component);
    const stat = lstatSync(current);
    if (stat.isSymbolicLink() || current !== path && !stat.isDirectory()) throw new Error("linked Pi evidence path");
    if (current === path && (stat.uid !== uid || (stat.mode & 0o777) !== mode || (directory ? !stat.isDirectory() : !stat.isFile()))) throw new Error("unowned Pi evidence path");
  }
  return lstatSync(path);
}

function sameFile(a, b) { return a.dev === b.dev && a.ino === b.ino; }

export function createPiEvidenceWriter(root, uid = process.getuid()) {
  privatePath(root, true, uid, 0o700);
  const directory = join(root, "evidence-pi");
  const directoryIdentity = privatePath(directory, true, uid, 0o700);
  const lockPath = `${directory}.writer`;
  const lock = openSync(lockPath, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW, 0o600);
  const lockIdentity = fstatSync(lock);
  let closed = false;
  const binding = () => {
    if (closed || !sameFile(directoryIdentity, privatePath(directory, true, uid, 0o700)) || !sameFile(lockIdentity, privatePath(lockPath, false, uid, 0o600))) throw new Error("Pi evidence writer binding changed");
  };
  return {
    save(name, value) {
      binding();
      if (!/^[A-Za-z0-9][A-Za-z0-9._-]*\.json$/.test(name)) throw new Error("invalid Pi evidence filename");
      const bytes = Buffer.from(`${JSON.stringify(value)}\n`);
      if (bytes.length > fileLimit) throw new Error("Pi evidence file cap exceeded");
      let total = bytes.length;
      for (const name of readdirSync(directory)) {
        const stat = privatePath(join(directory, name), false, uid, 0o600);
        if (stat.size > fileLimit) throw new Error("existing Pi evidence exceeds file cap");
        total += stat.size;
      }
      if (total > rootLimit) throw new Error("Pi evidence partition exceeded");
      const fd = openSync(join(directory, name), constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW, 0o600);
      try {
        if (writeSync(fd, bytes) !== bytes.length) throw new Error("short Pi evidence write");
        fsyncSync(fd);
      } finally { closeSync(fd); }
    },
    close() {
      if (closed) return;
      binding();
      unlinkSync(lockPath);
      closed = true;
      closeSync(lock);
    },
  };
}

export function createPassiveObservers(input, output, { nowNs, wallMs, incarnation, pid, byteLimit = 256*1024, chunkLimit = 1024, outputByteLimit = 512*1024, onReceiverChunk, onOutputChunk, sourceKind = "controlled-observer" }) {
  const installedNs = nowNs();
  const initialBytesWritten = Number.isSafeInteger(output.bytesWritten) ? output.bytesWritten : null;
  let removedNs = null, failed = false, totalBytes = 0, droppedChunks = 0, outputCarry = "", outputCalls = 0;
  let inputSeq = 0, modeSeq = 0, outputBytes = 0, inputForwardCalls = 0, outputForwardCalls = 0;
  let pendingInputBytes = 0, pendingOutputBytes = 0;
  const chunks = [], writes = [], modes = [], samples = [], errors = [];
  const failure = error => {
    failed = true;
    if (errors.length < 32) errors.push(errorText(error));
  };
  const common = { source_kind: sourceKind, incarnation, pid, listenerInstalledNs: installedNs };
  const receiverRecord = () => envelope("p1-pi-received-bytes", { ...common, listenerRemovedNs: removedNs, firstSeq: chunks[0]?.seq ?? inputSeq+1, lastSeq: chunks.at(-1)?.seq ?? inputSeq, chunks: structuredClone(chunks), droppedChunks, totalBytes, inputForwardCalls, overflow: failed, errors: errors.slice() }, !failed);
  const outputRecord = () => envelope("p1-pi-output-modes", { ...common, observerRemovedNs: removedNs, initialBytesWritten, installedBeforeFirstWrite: initialBytesWritten === null ? null : initialBytesWritten === 0, forwardedOnce: outputForwardCalls === outputCalls, outputCalls, outputForwardCalls, outputBytes, writes: structuredClone(writes), events: structuredClone(modes), overflow: failed, errors: errors.slice() }, !failed);
  const flushReceiver = () => {
    if (!onReceiverChunk || !chunks.length) return;
    try {
      onReceiverChunk(receiverRecord());
      chunks.length = 0;
      pendingInputBytes = 0;
    } catch (error) { failure(error); }
  };
  const flushOutput = () => {
    if (!onOutputChunk || !writes.length) return;
    try {
      onOutputChunk(outputRecord());
      writes.length = 0;
      pendingOutputBytes = 0;
    } catch (error) { failure(error); }
  };
  const receive = data => {
    const seq = ++inputSeq;
    try {
      const bytes = Buffer.from(data);
      if (chunks.length >= chunkLimit || totalBytes + bytes.length > byteLimit) { droppedChunks++; failure("receiver limit exceeded"); return; }
      const asciiExact = typeof data !== "string" || /^[\x00-\x7f]*$/.test(data);
      if (!asciiExact) failure("original input bytes unavailable after non-ASCII string decoding");
      chunks.push({ seq, hrtimeNs: nowNs(), wallMs: wallMs(), byteCount: bytes.length, base64: bytes.toString("base64"), asciiExact });
      totalBytes += bytes.length;
      pendingInputBytes += bytes.length;
    } catch (error) { droppedChunks++; failure(error); }
  };
  const originalEmit = input.emit;
  const emit = function (name, ...args) {
    if (name !== "data") return Reflect.apply(originalEmit, this, [name, ...args]);
    receive(args[0]);
    inputForwardCalls++;
    try { return Reflect.apply(originalEmit, this, [name, ...args]); }
    catch (error) { failure(error); throw error; }
    finally { if (pendingInputBytes >= 24*1024 || chunks.length >= 128) flushReceiver(); }
  };
  const originalWrite = output.write;
  const wrapper = function (...args) {
    const seq = ++outputCalls, hrtimeNs = nowNs(), wall = wallMs();
    let bytes = null;
    try {
      const encoding = typeof args[1] === "string" ? args[1] : "utf8";
      bytes = ArrayBuffer.isView(args[0]) ? Buffer.from(new Uint8Array(args[0].buffer, args[0].byteOffset, args[0].byteLength)) : Buffer.from(args[0], encoding);
    } catch (error) { failure(error); }
    let returned = null, writeError = null;
    outputForwardCalls++;
    try {
      const result = Reflect.apply(originalWrite, this, args);
      returned = typeof result === "boolean" ? result : null;
      return result;
    } catch (error) {
      writeError = errorText(error);
      failure(error);
      throw error;
    } finally {
      try {
        if (!bytes || outputBytes + bytes.length > outputByteLimit || writes.length >= chunkLimit) {
          failure("output bytes unavailable or output limit exceeded");
        } else {
          writes.push({ seq, hrtimeNs, wallMs: wall, byteCount: bytes.length, base64: bytes.toString("base64"), forwardCalls: 1, returned, error: writeError });
          outputBytes += bytes.length;
          pendingOutputBytes += bytes.length;
          const text = outputCarry + bytes.toString("latin1"), carryLength = outputCarry.length;
          for (const match of text.matchAll(/\x1b\[\?(2004|1006|1049)(h|l)/g)) {
            if (match.index + match[0].length <= carryLength) continue;
            if (modes.length >= 1024) { failure("output mode event limit exceeded"); break; }
            modes.push({ seq: ++modeSeq, writeSeq: seq, streamEnd: outputBytes - bytes.length - carryLength + match.index + match[0].length, hrtimeNs, mode: match[1] === "2004" ? "bracketed-paste" : match[1] === "1006" ? "sgr-mouse" : "alt-screen", enabled: match[2] === "h", sequenceBase64: Buffer.from(match[0], "latin1").toString("base64") });
          }
          outputCarry = text.slice(-16);
        }
      } catch (error) { failure(error); }
      if (pendingOutputBytes >= 24*1024 || writes.length >= 128) flushOutput();
    }
  };
  const dimensions = cause => {
    if (samples.length >= 256) { failure("terminal sample limit exceeded"); return; }
    samples.push({ seq: samples.length+1, hrtimeNs: nowNs(), wallMs: wallMs(), cause, columns: output.columns ?? null, rows: output.rows ?? null });
  };
  const resize = () => dimensions("resize");
  input.emit = emit;
  output.write = wrapper;
  output.on("resize", resize);
  dimensions("startup");
  return {
    flush() {
      flushReceiver();
      flushOutput();
    },
    snapshot() {
      const active = removedNs === null;
      if (input.emit !== (active ? emit : originalEmit) || output.write !== (active ? wrapper : originalWrite) || output.listeners("resize").includes(resize) !== active) failure("passive observer binding changed");
      return {
        receiver: receiverRecord(),
        output: outputRecord(),
        terminal: envelope("p1-pi-terminal-dimensions", { ...common, samples: structuredClone(samples), errors: errors.slice() }, !failed),
      };
    },
    remove() {
      if (removedNs !== null) return;
      removedNs = nowNs();
      flushReceiver();
      flushOutput();
      output.removeListener("resize", resize);
      if (input.emit === emit) input.emit = originalEmit;
      else failure("input emission observer replaced");
      if (output.write === wrapper) output.write = originalWrite;
      else failure("output write observer replaced");
    },
  };
}

function readSession(path, root) {
  if (!path.startsWith(`${join(root, "pi", "sessions")}${sep}`) || resolve(path) !== path) throw new Error("seed session escaped private Pi storage");
  privatePath(dirname(path), true, process.getuid(), 0o700);
  const stat = lstatSync(path);
  if (!stat.isFile() || stat.isSymbolicLink() || stat.uid !== process.getuid() || (stat.mode & 0o777) !== 0o600 || stat.nlink !== 1 || stat.size > fileLimit) throw new Error("unsafe seed session");
  const fd = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    const pinned = fstatSync(fd);
    if (!sameFile(stat, pinned)) throw new Error("seed session identity changed");
    const bytes = Buffer.alloc(fileLimit+1);
    let length = 0;
    for (;;) {
      const count = readSync(fd, bytes, length, bytes.length-length, null);
      if (!count) break;
      length += count;
      if (length > fileLimit) throw new Error("seed session grew beyond cap");
    }
    const after = fstatSync(fd), current = privatePath(path, false, process.getuid(), 0o600);
    if (!sameFile(pinned, after) || !sameFile(after, current) || pinned.uid !== process.getuid() || (pinned.mode & 0o777) !== 0o600 || pinned.nlink !== 1 || after.nlink !== 1 || current.nlink !== 1 || length !== pinned.size || ["size", "mtimeMs", "ctimeMs"].some(key => pinned[key] !== after[key] || after[key] !== current[key])) throw new Error("seed session changed while reading");
    return { bytes: bytes.subarray(0, length), identity: { dev: after.dev, ino: after.ino, uid: after.uid, mode: after.mode & 0o777, size: after.size, mtimeMs: after.mtimeMs, ctimeMs: after.ctimeMs } };
  } finally { closeSync(fd); }
}

export function installNativeEvidence() {
  const root = process.env.HERDR_P1_FIXTURE_ROOT;
  if (!root || process.env.PI_OFFLINE !== "1" || process.env.PI_CODING_AGENT_DIR !== join(root, "pi") || process.env.PI_EXPERIMENTAL === "1" || process.env.TMUX || !process.stdin.isTTY || !process.stdout.isTTY) throw new Error("missing isolated native Pi preload binding or unbounded startup probe enabled");
  const args = process.argv.slice(2), bridge = join(root, "pi", "bridge.mjs");
  const regular = ["--offline", "--tui-mode", "regular", "--extension", bridge];
  const automatic = ["--offline", "--extension", bridge];
  const incarnation = JSON.stringify(args) === JSON.stringify(regular) ? 1 : JSON.stringify(args) === JSON.stringify(automatic) ? 2 : 0;
  if (!incarnation || process.stdout.bytesWritten !== 0) throw new Error("Pi preload did not precede the first native output write");
  for (const name of ["HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"]) if (process.env[name]) throw new Error("ambient proxy refused in private Pi startup");
  const settingsPath = join(root, "pi", "settings.json");
  const settingsIdentity = privatePath(settingsPath, false, process.getuid(), 0o600);
  const settingsFD = openSync(settingsPath, constants.O_RDONLY | constants.O_NOFOLLOW);
  let settingsBytes;
  try {
    if (!sameFile(settingsIdentity, fstatSync(settingsFD)) || settingsIdentity.size > 4096) throw new Error("private Pi startup settings changed");
    settingsBytes = Buffer.alloc(settingsIdentity.size);
    if (readSync(settingsFD, settingsBytes, 0, settingsBytes.length, 0) !== settingsBytes.length) throw new Error("private Pi settings read incomplete");
    const after = fstatSync(settingsFD), named = privatePath(settingsPath, false, process.getuid(), 0o600);
    if (!sameFile(settingsIdentity, after) || !sameFile(after, named) || ["size", "mtimeMs", "ctimeMs"].some(key => settingsIdentity[key] !== after[key] || after[key] !== named[key])) throw new Error("private Pi settings changed while reading");
  } finally { closeSync(settingsFD); }
  const settings = JSON.parse(settingsBytes.toString("utf8"));
  const expectedSettingsKeys = incarnation === 1 ? ["enabledModels", "tuiMode"] : ["enabledModels", "lastChangelogVersion", "tuiMode"];
  if (!settings || typeof settings !== "object" || Array.isArray(settings) || JSON.stringify(Object.keys(settings).sort()) !== JSON.stringify(expectedSettingsKeys) || !["regular", "fullscreen"].includes(settings.tuiMode) || !Array.isArray(settings.enabledModels) || settings.enabledModels.length !== 0 || incarnation === 2 && settings.lastChangelogVersion !== "1.0.4") throw new Error("unexpected private Pi startup settings");
  const ownerKey = Symbol.for("herdr.p1.fixture.passiveEvidence");
  if (globalThis[ownerKey]) throw new Error("Pi evidence preload repeated");
  const writer = createPiEvidenceWriter(root), pid = process.pid;
  const state = { root, incarnation, pid, writer, providerCalls: 0, seedWritten: false, closed: false, quitObserved: false, widgetControl: null, overlayControl: null, checkpoint: 0, receiverChunk: 0, outputChunk: 0, lifecycle: [], errors: [] };
  const save = (name, value) => writer.save(name, value);
  state.observers = createPassiveObservers(process.stdin, process.stdout, {
    nowNs: () => process.hrtime.bigint().toString(), wallMs: Date.now, incarnation, pid, sourceKind: "real",
    onReceiverChunk: record => save(`receiver-chunk-${incarnation}-${String(++state.receiverChunk).padStart(4, "0")}.json`, record),
    onOutputChunk: record => save(`output-chunk-${incarnation}-${String(++state.outputChunk).padStart(4, "0")}.json`, record),
  });
  state.saveCheckpoint = () => {
    state.observers.flush();
    const suffix = `${incarnation}-${String(++state.checkpoint).padStart(3, "0")}`;
    for (const [kind, record] of Object.entries(state.observers.snapshot())) save(`${kind}-${suffix}.json`, record);
    save(`lifecycle-${suffix}.json`, envelope("p1-pi-lifecycle", { incarnation, pid, providerCalls: state.providerCalls, events: state.lifecycle.slice(), errors: state.errors.slice() }, !state.errors.length));
  };
  state.onExit = code => {
    if (state.closed) return;
    try {
      if (!state.quitObserved && state.errors.length < 32) state.errors.push("native quit shutdown was not observed before process exit");
      state.observers.remove();
      if (Object.values(state.observers.snapshot()).some(record => record.complete !== true) && state.errors.length < 32) state.errors.push("passive observer incomplete at process exit");
      state.saveCheckpoint();
      save(`exit-${incarnation}.json`, envelope("p1-pi-observer-exit", { incarnation, pid, code, observedNs: process.hrtime.bigint().toString(), receiverChunks: state.receiverChunk, outputChunks: state.outputChunk, checkpoints: state.checkpoint, providerCalls: state.providerCalls, quitObserved: state.quitObserved, errors: state.errors.slice(), boundary: "process-exit-listener after observed quit shutdown; not global descendant closure" }, code === 0 && state.quitObserved && !state.errors.length));
    } catch (error) {
      if (state.errors.length < 32) state.errors.push(errorText(error));
      process.exitCode = 1;
    } finally {
      state.observers.remove();
      writer.close();
      state.closed = true;
    }
  };
  globalThis[ownerKey] = state;
  process.once("exit", state.onExit);
  save(`startup-${incarnation}.json`, envelope("p1-pi-observer-startup", { incarnation, pid, argv: args, entryFile: process.argv[1], installedNs: process.hrtime.bigint().toString(), initialBytesWritten: process.stdout.bytesWritten, stdinReadableFlowing: process.stdin.readableFlowing, settingsPath, settingsSha256: sha256(settingsBytes), settingsBytesBase64: settingsBytes.toString("base64"), settingsIdentity: { dev: settingsIdentity.dev, ino: settingsIdentity.ino, uid: settingsIdentity.uid, mode: settingsIdentity.mode & 0o777, size: settingsIdentity.size, mtimeMs: settingsIdentity.mtimeMs, ctimeMs: settingsIdentity.ctimeMs }, settingsMode: settings.tuiMode, effectiveInitialMode: incarnation === 1 ? "regular" : settings.tuiMode, proxyEnvironmentAbsent: true, settingsProxyAbsent: !Object.hasOwn(settings, "httpProxy"), networkAbsenceProved: false }));
  return state;
}

export default function p1EvidenceExtension(pi) {
  const root = process.env.HERDR_P1_FIXTURE_ROOT;
  const state = globalThis[Symbol.for("herdr.p1.fixture.passiveEvidence")];
  if (!state || state.root !== root || state.pid !== process.pid || state.closed) throw new Error("native Pi evidence preload missing or changed");
  const { incarnation } = state;
  const { writer, lifecycle } = state;
  const event = (name, details) => {
    if (lifecycle.length >= 256) throw new Error("Pi lifecycle evidence cap exceeded");
    lifecycle.push({ seq: lifecycle.length+1, hrtimeNs: process.hrtime.bigint().toString(), event: name, details });
  };
  const saveCheckpoint = state.saveCheckpoint;
  pi.on("before_provider_request", () => {
    state.providerCalls++;
    if (state.errors.length < 32) state.errors.push("provider request observed in user-only fixture");
    throw new Error("provider request observed in P1 evidence fixture");
  });
  let restoreWidgetObserver;
  pi.on("session_start", (_event, ctx) => {
    event("session_start", { sessionId: ctx.sessionManager.getSessionId(), sessionFile: ctx.sessionManager.getSessionFile() });
    const ui = ctx.ui, originalSetWidget = ui.setWidget;
    const identities = new WeakMap();
    let identityCount = 0, lastObservation;
    const identity = component => {
      if (!component) return null;
      if (!identities.has(component)) identities.set(component, ++identityCount);
      return identities.get(component);
    };
    const wrappedSetWidget = function (key, factory, options) {
      if (key !== "herdr-mobile-relay:scroll-state" || typeof factory !== "function") return Reflect.apply(originalSetWidget, this, [key, factory, options]);
      const observedFactory = function (...args) {
        const component = Reflect.apply(factory, this, args), tui = args[0], originalRender = component.render;
        component.render = function (...renderArgs) {
          let before;
          try { before = tui.getFocusedComponent(); }
          catch (error) { if (state.errors.length < 32) state.errors.push(errorText(error)); }
          const rows = Reflect.apply(originalRender, this, renderArgs);
          try {
            const renderedRows = rows.length;
            if (state.widgetControl?.component === component) state.widgetControl.renderedRows = renderedRows;
            const observation = { renderer: tui.mode, width: renderArgs[0], renderedRows, focusBefore: identity(before), focusAfter: identity(tui.getFocusedComponent()), widgetIsFocused: before === component, widgetHasInputHandler: typeof component.handleInput === "function", widgetHasMouseHandler: typeof component.handleMouse === "function" };
            const signature = JSON.stringify(observation);
            if (signature !== lastObservation) {
              event("renderer_mode", observation);
              lastObservation = signature;
            }
          } catch (error) { if (state.errors.length < 32) state.errors.push(errorText(error)); }
          return rows;
        };
        state.widgetControl = { ui, key, factory, options, tui, component, installed: true, renderedRows: null };
        return component;
      };
      return Reflect.apply(originalSetWidget, this, [key, observedFactory, options]);
    };
    ui.setWidget = wrappedSetWidget;
    restoreWidgetObserver = () => {
      if (ui.setWidget !== wrappedSetWidget) {
        if (state.errors.length < 32) state.errors.push("native widget observer binding changed");
        return;
      }
      ui.setWidget = originalSetWidget;
    };
  });
  const controlRefusal = message => {
    if (state.errors.length < 32) state.errors.push(message);
    event("control_refusal", { message });
    throw new Error(message);
  };
  const controlContext = (args, ctx) => {
    if (args.trim() || !ctx.isIdle() || ctx.mode !== "tui") controlRefusal("native UI control context refused");
  };
  pi.registerCommand("p1-evidence-overlay-open", {
    description: "Open a generated noncapturing native overlay control",
    handler: async (args, ctx) => {
      controlContext(args, ctx);
      if (state.overlayControl) controlRefusal("native overlay control already active");
      const pending = ctx.ui.custom((tui, _theme, _keys, done) => {
        if (tui.hasOverlayEntries) controlRefusal("unowned native overlay entries are present");
        state.overlayControl = { tui, close: done, focus: tui.getFocusedComponent(), handle: null };
        return { render: () => ["Generated test content: P1 overlay"], invalidate() {} };
      }, {
        overlay: true,
        overlayOptions: { nonCapturing: true, width: 38 },
        onHandle: handle => {
          const control = state.overlayControl;
          if (!control || !control.tui.hasOverlayEntries || control.tui.getFocusedComponent() !== control.focus) controlRefusal("native noncapturing overlay observation refused");
          control.handle = handle;
          event("overlay_open", { renderer: control.tui.mode, hasOverlayEntries: control.tui.hasOverlayEntries, focusUnchanged: control.tui.getFocusedComponent() === control.focus, nonCapturing: true });
        },
      });
      void pending.catch(error => {
        if (state.errors.length < 32) state.errors.push(errorText(error));
      });
    },
  });
  pi.registerCommand("p1-evidence-overlay-close", {
    description: "Close the owned generated native overlay control",
    handler: async (args, ctx) => {
      controlContext(args, ctx);
      const control = state.overlayControl;
      if (!control?.handle || !control.tui.hasOverlayEntries) controlRefusal("native overlay control closure refused");
      control.close(undefined);
      state.overlayControl = null;
      event("overlay_close", { renderer: control.tui.mode, hasOverlayEntries: control.tui.hasOverlayEntries });
      if (control.tui.hasOverlayEntries) controlRefusal("native overlay entries remain after owned closure");
    },
  });
  pi.registerCommand("p1-evidence-widget-remove", {
    description: "Remove the actual owned zero-row bridge widget control",
    handler: async (args, ctx) => {
      controlContext(args, ctx);
      const control = state.widgetControl;
      if (!control?.installed || control.renderedRows !== 0 || typeof control.component.handleInput === "function" || typeof control.component.handleMouse === "function") controlRefusal("native zero-row widget observation refused");
      control.ui.setWidget(control.key, undefined, control.options);
      control.installed = false;
      event("widget_removed", { renderer: control.tui.mode, renderedRowsBefore: control.renderedRows, focusIsWidget: control.tui.getFocusedComponent() === control.component });
    },
  });
  pi.registerCommand("p1-evidence-widget-restore", {
    description: "Restore the actual native bridge widget factory",
    handler: async (args, ctx) => {
      controlContext(args, ctx);
      const control = state.widgetControl;
      if (!control || control.installed) controlRefusal("native widget restoration state refused");
      control.ui.setWidget(control.key, control.factory, control.options);
      if (!state.widgetControl?.installed || state.widgetControl === control) controlRefusal("native widget factory was not recreated");
      event("widget_restored", { renderer: state.widgetControl.tui.mode, renderedRows: state.widgetControl.renderedRows });
    },
  });
  pi.registerCommand("p1-evidence-seed", {
    description: "Create generated user-only P1 qualification history",
    handler: async (args, ctx) => {
      if (args.trim() || state.seedWritten || state.providerCalls || !ctx.isIdle() || ctx.mode !== "tui") throw new Error("seed command state refused");
      const previousSessionId = ctx.sessionManager.getSessionId(), previousSessionFile = ctx.sessionManager.getSessionFile();
      const entryIds = [];
      const result = await ctx.newSession({
        setup: async manager => {
          for (let number = 1; number <= 96; number++) {
            const marker = `P1 TEST USER [${String(number).padStart(3, "0")}]`;
            entryIds.push(manager.appendMessage({ role: "user", content: `${marker} generated test content\nGenerated test content for terminal and Conversation History qualification.`, timestamp: Date.now() }));
          }
        },
        withSession: async fresh => {
          const sessionFile = fresh.sessionManager.getSessionFile();
          const sessionId = fresh.sessionManager.getSessionId();
          if (!sessionFile || !sessionId || sessionId === previousSessionId || sessionFile === previousSessionFile || !fresh.isIdle() || fresh.mode !== "tui" || !fresh.sessionManager.isPersisted()) throw new Error("seed replacement identity refused");
          const { bytes, identity: sessionFileIdentity } = readSession(sessionFile, root);
          const records = bytes.toString("utf8").trimEnd().split("\n").map(line => JSON.parse(line));
          const entries = records.filter(entry => entry.type === "message");
          if (records[0]?.type !== "session" || records[0].id !== sessionId || records[0].cwd !== join(root, "work") || entries.length !== 96 || new Set(entryIds).size !== 96 || entries.some((entry, index) => entry.id !== entryIds[index] || index > 0 && entry.parentId !== entryIds[index-1] || entry.message.role !== "user" || entry.message.content !== `P1 TEST USER [${String(index+1).padStart(3, "0")}] generated test content\nGenerated test content for terminal and Conversation History qualification.`) || state.providerCalls) throw new Error("Pi-written user seed mismatch");
          writer.save(`seed-${incarnation}.json`, envelope("p1-pi-session-seed", { incarnation, pid: state.pid, sessionFile, sessionId, previousSessionId, previousSessionFile, sessionFileIdentity, entries: entries.length, entryIds, role: "user", label: "generated test content", firstMarker: "P1 TEST USER [001]", lastMarker: "P1 TEST USER [096]", sessionFileSha256AfterSeed: sha256(bytes), providerCalls: state.providerCalls }));
          state.seedWritten = true;
          event("session_replaced", { sessionFile, sessionId });
        },
      });
      if (result.cancelled || !state.seedWritten) throw new Error("Pi seed session replacement incomplete");
    },
  });
  pi.registerCommand("p1-evidence-checkpoint", { description: "Retain passive P1 observations", handler: async args => { if (args.trim()) throw new Error("unexpected checkpoint arguments"); saveCheckpoint(); } });
  pi.on("session_shutdown", shutdown => {
    if (state.closed) return;
    event(shutdown.reason === "reload" ? "reload" : "shutdown", { reason: shutdown.reason, targetSessionFile: shutdown.targetSessionFile ?? null });
    if (state.overlayControl) {
      state.overlayControl.close(undefined);
      state.overlayControl = null;
    }
    restoreWidgetObserver?.();
    restoreWidgetObserver = undefined;
    state.widgetControl = null;
    saveCheckpoint();
    if (shutdown.reason !== "quit") return;
    state.quitObserved = true;
    process.removeListener("exit", state.onExit);
    process.once("exit", state.onExit);
  });
}
