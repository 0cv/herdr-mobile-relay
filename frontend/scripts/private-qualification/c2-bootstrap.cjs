'use strict';

const fs = module.require('node:fs');
const path = module.require('node:path');
const crypto = module.require('node:crypto');
const privateCompletion = module.require('./privateCompletion.cjs');
const { createPrivateRoute } = module.require('./c2-route.cjs');
const { fileURLToPath, pathToFileURL } = module.require('node:url');
const builtinModules = new Set(module.require('node:module').builtinModules);

let runtime;
let dynamicSites = new Map();
let supportConfigured = false;

function sha256(value) {
  return crypto.createHash('sha256').update(value).digest('hex');
}

function readPinnedJson(filename, expectedUid) {
  const before = fs.lstatSync(filename);
  if (!before.isFile() || before.isSymbolicLink() || Number(before.uid) !== expectedUid || Number(before.nlink) !== 1 || (Number(before.mode) & 0o077) !== 0 || fs.realpathSync(filename) !== filename)
    throw new Error(`Private invocation file identity refused: ${filename}`);
  const bytes = fs.readFileSync(filename);
  const after = fs.lstatSync(filename);
  if (before.dev !== after.dev || before.ino !== after.ino || before.size !== after.size || before.mtimeMs !== after.mtimeMs || bytes.length !== before.size)
    throw new Error(`Private invocation file changed while reading: ${filename}`);
  return {
    value: JSON.parse(bytes.toString('utf8')),
    sha256: sha256(bytes),
    bytes: bytes.length,
    identity: { dev: String(after.dev), ino: String(after.ino), uid: Number(after.uid), mode: Number(after.mode), nlink: Number(after.nlink), size: Number(after.size), file: after.isFile(), directory: after.isDirectory(), symlink: after.isSymbolicLink() }
  }
}

function readInvocationFromEnvironment(env = process.env) {
  const filename = env.HERDR_C2_INVOCATION;
  if (!filename || !path.isAbsolute(filename))
    throw new Error('Private invocation descriptor is not configured');
  const expectedRoot = path.resolve(env.HERDR_C2_INVOCATION_ROOT || '');
  const actual = path.resolve(filename);
  if (!expectedRoot || path.dirname(actual) !== expectedRoot || path.basename(actual) !== 'invocation.json')
    throw new Error('Private invocation descriptor path is not bound');
  const uid = process.getuid?.();
  if (!Number.isInteger(uid)) throw new Error('Private process UID is unavailable');
  const read = readPinnedJson(actual, uid);
  const descriptor = read.value;
  if (descriptor.schema !== 1 || descriptor.invocationId !== path.basename(expectedRoot).replace(/^invocation-/, '') || descriptor.root !== expectedRoot || descriptor.uid !== uid || descriptor.nativeReady !== false || descriptor.configSHA256 !== sha256(fs.readFileSync(descriptor.configPath)))
    throw new Error('Private invocation descriptor does not match its source identity');
  return { descriptor, identity: { path: actual, sha256: read.sha256, bytes: read.bytes } };
}

function disabledAdapters(route, descriptor) {
  const timers = new Map();
  let timerId = 0;
  return {
    now: () => Number(process.hrtime.bigint()) / 1e6,
    deadline: () => Number.isFinite(descriptor.deadline) ? descriptor.deadline : Number.MAX_SAFE_INTEGER,
    setTimer(callback, delay) {
      const id = ++timerId;
      const timer = setTimeout(() => {
        timers.delete(id);
        callback();
      }, Math.max(0, delay));
      timers.set(id, timer);
      return id;
    },
    clearTimer(id) {
      clearTimeout(timers.get(id));
      timers.delete(id);
    },
    publish(outcome) {
      route.appendOutcome({ type: 'completion', invocationId: descriptor.invocationId, outcome });
    },
    admitWorker(params) {
      return route.admitWorker(params);
    },
    retainWorker(params) {
      return route.retainWorker(params);
    },
    requestOwnedForce() {
      throw new Error('C2 has no native owned-force authority');
    }
  };
}

function loadDynamicManifest(descriptor, expectedUid) {
  if (!descriptor.dynamicManifestPath || !descriptor.dynamicManifestSHA256)
    throw new Error('Private dynamic-module manifest is not bound to this invocation');
  const manifestRead = readPinnedJson(descriptor.dynamicManifestPath, expectedUid);
  if (manifestRead.sha256 !== descriptor.dynamicManifestSHA256)
    throw new Error('Private dynamic-module manifest digest differs from the invocation binding');
  const manifest = manifestRead.value;
  if (manifest.schema !== 1 || manifest.invocationId !== descriptor.invocationId || manifest.root !== descriptor.root || !Array.isArray(manifest.sites))
    throw new Error('Private dynamic-module manifest is not bound to this invocation');
  const sites = new Map();
  for (const site of manifest.sites) {
    if (!/^DS(?:0[1-9]|[12][0-9]|3[0-7])$/.test(site.siteId) || sites.has(site.siteId) || !['bound', 'refused'].includes(site.disposition))
      throw new Error('Private dynamic-module manifest contains an invalid site');
    if (!Array.isArray(site.approvedTargets) || !Array.isArray(site.enforcedBy))
      throw new Error(`Private dynamic-module target table is malformed: ${site.siteId}`);
    for (const target of site.approvedTargets) {
      if (typeof target.specifier !== 'string' || !target.specifier || target.builtin !== true && (!/^[a-f0-9]{64}$/.test(target.sha256) || typeof target.resolvedPath !== 'string' || path.isAbsolute(target.resolvedPath) || target.resolvedPath.split('/').some(part => !part || part === '.' || part === '..')))
        throw new Error(`Private dynamic-module target is malformed: ${site.siteId}`);
      if (target.builtin === true && !builtinModules.has(target.specifier.replace(/^node:/, '')))
        throw new Error(`Private dynamic-module builtin is not approved: ${site.siteId}`);
      if (target.builtin !== true && path.resolve(descriptor.runtimeRoot, target.resolvedPath) !== path.resolve(target.specifier))
        throw new Error(`Private dynamic-module target is not bound to its runtime path: ${site.siteId}`);
    }
    if (site.disposition === 'refused' && (site.approvedTargets.length || site.enforcedBy.length || site.refusal?.errorName !== 'C2DynamicModuleRefusalError'))
      throw new Error(`Private dynamic-module refusal is malformed: ${site.siteId}`);
    if (site.disposition === 'bound' && !site.approvedTargets.length && !site.enforcedBy.length)
      throw new Error(`Private dynamic-module site has no bound target: ${site.siteId}`);
    sites.set(site.siteId, site);
  }
  if (sites.size !== 37) throw new Error('Private dynamic-module manifest does not cover all inventoried sites');
  return sites;
}

function dynamicRefusal(siteId, reason) {
  const error = new Error(`C2 dynamic module refused at ${siteId}: ${reason}`);
  error.name = 'C2DynamicModuleRefusalError';
  error.siteId = siteId;
  return error;
}

function normalizeDynamicSpecifier(value, fromFile) {
  if (typeof value !== 'string' || !value || value.includes('\0'))
    throw new Error('specifier is not a non-empty string');
  if (value.startsWith('file:')) return path.resolve(fileURLToPath(new URL(value)));
  if (value.startsWith('node:')) return value;
  if (builtinModules.has(value)) return `node:${value}`;
  if (path.isAbsolute(value)) return path.resolve(value);
  if (value.startsWith('./') || value.startsWith('../'))
    return path.resolve(path.dirname(fromFile), value);
  return value;
}

function dynamicModule(siteId, kind, specifier, fromFile) {
  if (!runtime || !['require', 'import'].includes(kind))
    throw dynamicRefusal(siteId, 'runtime or loader kind is unavailable');
  const site = dynamicSites.get(siteId);
  if (!site) throw dynamicRefusal(siteId, 'site is not present in the bound manifest');
  if (site.disposition !== 'bound') throw dynamicRefusal(siteId, 'site is unsupported by the private route');
  if (typeof fromFile !== 'string' || !path.isAbsolute(fromFile))
    throw dynamicRefusal(siteId, 'caller path is not absolute');
  let computed;
  try {
    computed = typeof specifier === 'function' ? specifier() : specifier;
  } catch {
    throw dynamicRefusal(siteId, 'specifier computation failed');
  }
  let normalized;
  try {
    normalized = normalizeDynamicSpecifier(computed, fromFile);
  } catch (error) {
    throw dynamicRefusal(siteId, error.message);
  }
  const target = (site.approvedTargets || []).find(item => item.specifier === normalized);
  if (!target) throw dynamicRefusal(siteId, `specifier is not approved: ${normalized}`);
  if (target.builtin === true) return target.specifier;
  const runtimeRoot = path.resolve(runtime.descriptor.runtimeRoot || path.join(runtime.descriptor.root, 'runtime'));
  const resolvedPath = path.resolve(runtimeRoot, target.resolvedPath);
  if (resolvedPath !== runtimeRoot && !resolvedPath.startsWith(`${runtimeRoot}${path.sep}`))
    throw dynamicRefusal(siteId, 'approved target escaped the private runtime');
  if (target.projectInput === true) {
    let inputs;
    try {
      inputs = runtime.route.dynamicProjectInputs();
    } catch {
      throw dynamicRefusal(siteId, 'project input route has not been validated');
    }
    if (inputs.get(resolvedPath) !== target.sha256)
      throw dynamicRefusal(siteId, 'project input is not admitted by the validated route');
  }
  return kind === 'import' ? pathToFileURL(resolvedPath).href : resolvedPath;
}

function enforceDynamicSite(siteId, enforcedBy) {
  const site = dynamicSites.get(siteId);
  if (!site || site.disposition !== 'bound' || !Array.isArray(site.enforcedBy) || JSON.stringify(site.enforcedBy) !== JSON.stringify(enforcedBy))
    throw dynamicRefusal(siteId, 'delegation does not match the bound manifest');
  return true;
}

function guardTransformedCode(source, filename, isModule) {
  if (typeof source !== 'string' || typeof filename !== 'string' || !path.isAbsolute(filename))
    throw new Error('Transformed module identity is not bound');
  const calls = [];
  const expression = /\b(require|import)\s*\(\s*([A-Za-z_$][\w$]*)\s*\)/g;
  for (const match of source.matchAll(expression)) calls.push({ start: match.index, end: match.index + match[0].length, kind: match[1], variable: match[2], siteId: match[1] === 'require' ? 'DS23' : 'DS25' });
  const openCalls = [...source.matchAll(/\b(?:require|import)\s*\(/g)];
  const computedCalls = openCalls.filter(match => {
    let index = match.index + match[0].length;
    while (/\s/.test(source[index] || '')) index += 1;
    return source[index] !== ')' && source[index] !== "'" && source[index] !== '"';
  });
  if (computedCalls.length !== calls.length)
    throw new Error('Transformed code contains an unsupported computed module expression');
  if (!calls.length) return source;
  const bootstrapPath = path.join(path.dirname(runtime.descriptor.dynamicManifestPath), 'c2-bootstrap.cjs');
  let guarded = source;
  for (const call of calls.reverse()) {
    const replacement = `${call.kind}(__herdrC2.dynamicModule(${JSON.stringify(call.siteId)}, ${JSON.stringify(call.kind)}, () => ${call.variable}, ${JSON.stringify(filename)}))`;
    guarded = guarded.slice(0, call.start) + replacement + guarded.slice(call.end);
  }
  const binding = isModule
    ? `import __herdrC2 from ${JSON.stringify(pathToFileURL(bootstrapPath).href)};\n`
    : `const __herdrC2 = require(${JSON.stringify(bootstrapPath)});\n`;
  return binding + guarded;
}

function installSourceRuntime(options) {
  if (runtime) throw new Error('Private C2 bootstrap was configured more than once');
  if (!options || options.nativeEffectsDisabled !== true || !options.paths || !options.descriptor)
    throw new Error('C2 source runtime requires explicit disabled native effects and enrolled paths');
  const descriptor = options.descriptor;
  if (descriptor.nativeReady !== false || descriptor.invocationId !== options.paths.id || descriptor.root !== options.paths.root || descriptor.uid !== options.paths.uid)
    throw new Error('C2 source runtime identity does not match its path enrollment');
  const route = options.route || createPrivateRoute(options.paths, descriptor);
  if (route.paths !== options.paths || route.descriptor.invocationId !== descriptor.invocationId)
    throw new Error('C2 source route is not bound to this invocation');
  descriptor.internalCacheDir = route.transformCacheDir();
  if (!supportConfigured) {
    privateCompletion.configure(disabledAdapters(route, descriptor));
    supportConfigured = true;
  }
  dynamicSites = loadDynamicManifest(descriptor, descriptor.uid);
  route.validateDynamicProjectInputs();
  runtime = { descriptor, paths: options.paths, route, completion: privateCompletion, nativeEffectsDisabled: true, nativeReady: false, dynamicSites };
  return runtime;
}

function assertExecutionReady() {
  if (!runtime || runtime.nativeEffectsDisabled || runtime.nativeReady !== true)
    throw new Error('C2 private execution remains closed pending C3/C4 readiness');
  return runtime;
}

function assertPrivateModeArguments(argv) {
  if (!Array.isArray(argv) || argv.some(argument => typeof argument !== 'string'))
    throw new Error('Private CLI arguments are malformed');
  const unsupported = new Set(['--ui', '--ui-host', '--ui-port', '--watch', '--watch-all']);
  if (argv.some(argument => unsupported.has(argument.split('=')[0])))
    throw new Error('Private UI and watch modes are not supported');
}

function beforeProgramLoad(argv = process.argv.slice(2)) {
  assertPrivateModeArguments(argv);
  if (runtime) return assertExecutionReady();
  const { descriptor } = readInvocationFromEnvironment();
  const readinessPath = path.join(descriptor.root, 'native-readiness.json');
  let readiness;
  try {
    readiness = readPinnedJson(readinessPath, descriptor.uid).value;
  } catch {
    throw new Error('Private Playwright entry refused before program require: C3/C4 readiness is absent');
  }
  if (readiness.schema !== 1 || readiness.invocationId !== descriptor.invocationId || readiness.C3 !== 'complete' || readiness.C4 !== 'complete' || readiness.nativeReady !== true)
    throw new Error('Private Playwright entry refused before program require: C3/C4 readiness is invalid');
  throw new Error('C2 private execution remains closed pending independent C3/C4 qualification');
}

function beforeWorkerLoad() {
  if (runtime) return assertExecutionReady();
  const { descriptor } = readInvocationFromEnvironment();
  if (descriptor.nativeReady !== true)
    throw new Error('Private worker entry refused before common module load: C3/C4 readiness is absent');
  const readinessPath = path.join(descriptor.root, 'native-readiness.json');
  const readiness = readPinnedJson(readinessPath, descriptor.uid).value;
  if (readiness.schema !== 1 || readiness.invocationId !== descriptor.invocationId || readiness.C3 !== 'complete' || readiness.C4 !== 'complete' || readiness.nativeReady !== true)
    throw new Error('Private worker entry refused before common module load: C3/C4 readiness is invalid');
  const manifestPath = descriptor.routeManifestPath;
  const manifestRead = readPinnedJson(manifestPath, descriptor.uid);
  const manifest = manifestRead.value;
  if (manifest.schema !== 1 || manifest.invocationId !== descriptor.invocationId || manifest.root !== descriptor.root || manifest.uid !== descriptor.uid)
    throw new Error('Private worker route manifest is not bound to its parent invocation');
  const pathTools = module.require('./c2-paths.cjs');
  const stage = pathTools.readStageRoot(descriptor.stageRoot);
  const routeManifestEntry = {
    path: path.relative(descriptor.root, manifestPath),
    kind: 'file',
    dev: manifestRead.identity.dev,
    ino: manifestRead.identity.ino,
    uid: manifestRead.identity.uid,
    mode: manifestRead.identity.mode,
    nlink: manifestRead.identity.nlink,
    bytes: manifestRead.identity.size,
    hash: manifestRead.sha256
  };
  const paths = pathTools.attachInvocation(stage, descriptor.root, descriptor.invocationId, manifest.entries, [routeManifestEntry]);
  const routeModule = module.require('./c2-route.cjs');
  const route = routeModule.createPrivateRoute(paths, descriptor, { processToken: crypto.randomUUID() });
  route.validateSerializedRoute({ __herdrC2Route: manifest.route });
  installSourceRuntime({ nativeEffectsDisabled: true, paths, descriptor, route });
  return assertExecutionReady();
}

function getPrivateCompletion() {
  return privateCompletion;
}

function getRuntime() {
  if (!runtime) throw new Error('Private C2 runtime is not configured');
  return runtime;
}

module.exports = { beforeProgramLoad, beforeWorkerLoad, installSourceRuntime, assertExecutionReady, readInvocationFromEnvironment, readPinnedJson, getPrivateCompletion, getRuntime, dynamicModule, enforceDynamicSite, dynamicRefusal, guardTransformedCode };
