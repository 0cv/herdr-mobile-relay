'use strict';

const fs = module.require('node:fs');
const path = module.require('node:path');
const crypto = module.require('node:crypto');

const forbiddenKeys = new Set([
  'outputDir', 'lastRun', 'lastRunFile', 'cacheDir', 'ctCacheDir', 'userDataDir',
  'downloadsPath', 'artifactsDir', 'traceDir', 'profileDir', 'storageState',
  'videoDir', 'screenshotDir', 'snapshotPathTemplate', 'preserveOutputDir', 'proxy'
]);
const allowedProjectNames = new Set(['chromium-mobile', 'webkit-mobile']);

function digestFile(filename) {
  return crypto.createHash('sha256').update(fs.readFileSync(filename)).digest('hex');
}

function cleanId(value) {
  if (typeof value !== 'string' || !/^[a-zA-Z0-9][a-zA-Z0-9_-]{0,79}$/.test(value))
    throw new Error(`Private project identity is invalid: ${String(value)}`);
  return value;
}

function inspectPathOptions(value, location = 'config') {
  if (!value || typeof value !== 'object') return;
  if (Array.isArray(value)) {
    value.forEach((item, index) => inspectPathOptions(item, `${location}[${index}]`));
    return;
  }
  for (const [key, item] of Object.entries(value)) {
    if (forbiddenKeys.has(key)) throw new Error(`Unsupported private path option: ${location}.${key}`);
    if (key === 'trace' && item !== undefined && item !== 'retain-on-failure')
      throw new Error(`Unsupported private trace option: ${location}.trace`);
    inspectPathOptions(item, `${location}.${key}`);
  }
}

function validateReporter(value, label) {
  if (value === undefined) return;
  const names = typeof value === 'string' ? [value] : Array.isArray(value) ? value.map(entry => Array.isArray(entry) ? entry[0] : entry) : [];
  if (!names.length || names.some(name => name !== 'list' && name !== 'dot'))
    throw new Error(`Unsupported private reporter: ${label}`);
}

function assertSafeArguments(argv, boundConfig) {
  if (!Array.isArray(argv) || argv.some(arg => typeof arg !== 'string'))
    throw new Error('Private CLI arguments are malformed');
  for (let index = 0; index < argv.length; index += 1) {
    const arg = argv[index];
    if (['--ui', '--ui-host', '--ui-port', '--watch', '--watch-all'].includes(arg.split('=')[0]))
      throw new Error('Private UI and watch modes are not supported');
    if (arg === '--output' || arg === '-o' || arg.startsWith('--output='))
      throw new Error('Private output override is not supported');
    if (arg === '--config' || arg.startsWith('--config=')) {
      const value = arg.includes('=') ? arg.slice(arg.indexOf('=') + 1) : argv[index + 1];
      if (value !== boundConfig && path.resolve(value || '') !== boundConfig)
        throw new Error('Private config path is not bound to this invocation');
      if (arg === '--config') index += 1;
    }
  }
}

function validateEnvironment(env, descriptor) {
  if (env.PWTEST_CACHE_DIR && path.resolve(env.PWTEST_CACHE_DIR) !== path.resolve(descriptor.internalCacheDir || ''))
    throw new Error('Unsupported private transform-cache override');
  for (const key of ['PLAYWRIGHT_LAST_RUN_OUTPUT_FILE', 'PLAYWRIGHT_HTML_OUTPUT_DIR', 'PLAYWRIGHT_JUNIT_OUTPUT_NAME'])
    if (env[key]) throw new Error(`Unsupported private writer override: ${key}`);
  if (env.HERDR_WEB_ROOT && descriptor.webRoot && path.resolve(env.HERDR_WEB_ROOT) !== path.resolve(descriptor.webRoot))
    throw new Error('Private web root is not bound to this invocation');
}

function effectiveOutput(config, project, cli, packageJsonDir) {
  if (cli.outputDir !== undefined) return cli.outputDir;
  if (project.outputDir !== undefined) return path.resolve(config.configDir, project.outputDir);
  if (config.outputDir !== undefined) return path.resolve(config.configDir, config.outputDir);
  return path.join(packageJsonDir, 'test-results');
}

function validateOriginalOptions(input) {
  const { location, userConfig, configCLIOverrides, packageJsonDir, argv, env, descriptor } = input;
  if (!location || !userConfig || !configCLIOverrides || !descriptor)
    throw new Error('Private route validation inputs are incomplete');
  const configPath = path.resolve(location.resolvedConfigFile || path.join(location.configDir, 'playwright.config.ts'));
  if (configPath !== path.resolve(descriptor.configPath))
    throw new Error('Private config file identity is not bound');
  if (digestFile(configPath) !== descriptor.configSHA256)
    throw new Error('Private config file content changed');
  assertSafeArguments(argv || configCLIOverrides.argv || [], configPath);
  validateEnvironment(env || process.env, descriptor);
  inspectPathOptions(userConfig);
  inspectPathOptions(configCLIOverrides, 'cli');
  if (configCLIOverrides.outputDir !== undefined)
    throw new Error('Private outputDir CLI override is not supported');
  if (configCLIOverrides.reporter !== undefined)
    validateReporter(configCLIOverrides.reporter, 'CLI');
  validateReporter(userConfig.reporter, 'config');
  const testConfig = userConfig['@playwright/test'];
  if (testConfig?.plugins?.length)
    throw new Error('Private custom plugins are not supported');
  if (userConfig.globalSetup || userConfig.globalTeardown)
    throw new Error('Private global setup and teardown hooks are not supported');
  const servers = userConfig.webServer === undefined ? [] : Array.isArray(userConfig.webServer) ? userConfig.webServer : [userConfig.webServer];
  if (servers.length > 1 || servers.some(server => !server || typeof server.command !== 'string' || !/^(?:node|bun) scripts\/browser-server\.mjs\b/.test(server.command) || server.reuseExistingServer !== false))
    throw new Error('Private web server is not the bound local fixture');
  const projects = userConfig.projects || [{ ...userConfig, workers: undefined }];
  if (!Array.isArray(projects) || projects.length !== 2)
    throw new Error('Private route requires the complete two-project configuration');
  const projectNames = projects.map(project => project?.name);
  if (new Set(projectNames).size !== 2 || projectNames.some(name => !allowedProjectNames.has(name)))
    throw new Error('Private project set differs from the bound Chromium/WebKit pair');
  const expectedBrowsers = { 'chromium-mobile': 'chromium', 'webkit-mobile': 'webkit' };
  if (projects.some(project => project.use?.defaultBrowserType !== expectedBrowsers[project.name]))
    throw new Error('Private browser project type differs from the bound Chromium/WebKit pair');
  const outputs = projects.map(project => ({
    id: cleanId(project.name),
    original: effectiveOutput(userConfig, project, configCLIOverrides, packageJsonDir)
  }));
  const defaultOutput = path.resolve(packageJsonDir, 'test-results');
  if (outputs.some(item => path.resolve(item.original) !== defaultOutput))
    throw new Error('Original project output options differ from the known default');
  const route = {
    schema: 1,
    invocationId: descriptor.invocationId,
    routeId: crypto.createHash('sha256').update(`${descriptor.invocationId}\0${configPath}\0${descriptor.configSHA256}`).digest('hex'),
    configPath,
    configSHA256: descriptor.configSHA256,
    projectIds: outputs.map(item => item.id),
    projectOutputs: {},
    projectPins: {},
    validated: true
  };
  const serialized = configCLIOverrides.__herdrC2Route;
  if (serialized) {
    if (serialized.invocationId !== route.invocationId || serialized.configPath !== route.configPath || serialized.configSHA256 !== route.configSHA256)
      throw new Error('Serialized private route differs from the original config identity');
    return assertRoute(serialized, descriptor);
  }
  Object.defineProperty(configCLIOverrides, '__herdrC2Route', { value: route, enumerable: true, configurable: false, writable: false });
  return route;
}

function assertRoute(route, descriptor) {
  if (!route || route.schema !== 1 || route.validated !== true || route.invocationId !== descriptor.invocationId || route.configPath !== path.resolve(descriptor.configPath) || route.configSHA256 !== descriptor.configSHA256 || route.routeId !== crypto.createHash('sha256').update(`${descriptor.invocationId}\0${route.configPath}\0${route.configSHA256}`).digest('hex'))
    throw new Error('Serialized private route is missing, mixed, or tampered');
  if (!Array.isArray(route.projectIds) || route.projectIds.length !== 2 || new Set(route.projectIds).size !== 2 || route.projectIds.some(id => !allowedProjectNames.has(id)))
    throw new Error('Serialized private project map is incomplete');
  for (const id of route.projectIds) {
    const expected = route.projectPins?.[id];
    const output = route.projectOutputs?.[id];
    if (!expected || output !== expected.path)
      throw new Error(`Serialized private project output is not bound: ${id}`);
    const stat = fs.lstatSync(output);
    if (!stat.isDirectory() || stat.isSymbolicLink() || Number(stat.uid) !== descriptor.uid || String(stat.dev) !== expected.dev || String(stat.ino) !== expected.ino || fs.realpathSync(output) !== output)
      throw new Error(`Serialized private project output identity changed: ${id}`);
  }
  return route;
}

function createPrivateRoute(paths, descriptor, options = {}) {
  if (!paths || !descriptor || descriptor.invocationId !== paths.id)
    throw new Error('Private route does not match the admitted invocation');
  let route;
  let configValidated = false;
  let dynamicProjectInputsValidated = false;
  let dynamicProjectInputs = new Map();
  const routeActor = 'private-route';
  paths.registerActor(routeActor, paths.root);
  const projectParent = paths.acquireSharedDirectory('projects', routeActor);
  const projectPins = {};
  function prepareProject(projectId) {
    const id = cleanId(projectId);
    paths.registerActor(`project:${id}`, projectParent.path);
    const result = paths.acquireSharedDirectory(`${id}/results`, `project:${id}`);
    projectPins[id] = { path: result.path, dev: result.identity.dev, ino: result.identity.ino };
    return result.path;
  }
  function domain(name, actor = `domain:${name}`) {
    paths.registerActor(actor, paths.root);
    return paths.acquireSharedDirectory(name, actor).path;
  }
  const fixedDomains = Object.fromEntries(['runtime', 'transform-cache', 'profiles', 'browser-artifacts', 'traces', 'uploads', 'downloads', 'logs', 'reports', 'control'].map(name => [name, domain(name)]));
  const outcomeActor = 'writer:control';
  paths.registerActor(outcomeActor, fixedDomains.control);
  const outcomeName = options.processToken ? `outcomes-${cleanId(options.processToken)}.ndjson` : 'outcomes.ndjson';
  const outcomeFile = paths.acquireFile(outcomeName, outcomeActor, '');
  const admittedWorkers = new Map();
  const launchedProfiles = new Set();
  const admittedTestOutputs = new Map();
  const admittedTraces = new Map();
  const admittedTemporaryPaths = new Map();
  const admittedUploadRoots = new Map();
  const artifactIntents = new Map();
  const launchDirectories = [];
  let profileOrdinal = 0;
  let artifactOrdinal = 0;
  let testOrdinal = 0;
  function outputFor(projectId) {
    const id = cleanId(projectId);
    const entry = projectPins[id];
    if (!entry) throw new Error(`Private project was not validated: ${id}`);
    return entry.path;
  }
  function writeRetainedFile(target, data, actor) {
    if (typeof actor !== 'string' || !actor) throw new Error('Private writer actor is required');
    const absolute = path.resolve(target);
    const parentPath = path.dirname(absolute);
    let base = [...paths.objects.entries()]
      .filter(([candidate, lease]) => lease.kind === 'directory' && (parentPath === candidate || parentPath.startsWith(`${candidate}${path.sep}`)))
      .sort((left, right) => right[0].length - left[0].length)[0]?.[0];
    if (!base) throw new Error(`Private writer parent has no enrolled scope: ${parentPath}`);
    if (!paths.actors.has(actor)) paths.registerActor(actor, base);
    else if (paths.actorBase(actor) !== base) throw new Error(`Private writer actor was rebound: ${actor}`);
    const relativeParent = path.relative(base, parentPath);
    if (relativeParent && relativeParent !== '.') paths.acquireSharedDirectory(relativeParent, actor);
    return paths.acquireFile(path.basename(absolute), actor, data).path;
  }
  function writeCacheFile(target, data, actor) {
    if (!configValidated) throw new Error('Transform-cache writes are deferred until all original project options pass');
    const cacheRoot = fixedDomains['transform-cache'];
    const absolute = path.resolve(target);
    if (absolute !== cacheRoot && !absolute.startsWith(`${cacheRoot}${path.sep}`))
      throw new Error('Transform-cache path escaped its private root');
    return writeRetainedFile(absolute, data, actor);
  }
  function admitWritableFile(target, actor) {
    const absolute = path.resolve(target);
    if (!absolute.startsWith(`${paths.root}${path.sep}`)) throw new Error('Private writable path escaped its invocation');
    const parentPath = path.dirname(absolute);
    const base = [...paths.objects.entries()]
      .filter(([candidate, lease]) => lease.kind === 'directory' && (parentPath === candidate || parentPath.startsWith(`${candidate}${path.sep}`)))
      .sort((left, right) => right[0].length - left[0].length)[0]?.[0];
    if (!base) throw new Error(`Private writable parent has no enrolled scope: ${parentPath}`);
    if (!paths.actors.has(actor)) paths.registerActor(actor, base);
    if (paths.actorBase(actor) !== base) throw new Error(`Private writer actor was rebound: ${actor}`);
    const relativeParent = path.relative(base, parentPath);
    if (relativeParent && relativeParent !== '.') paths.acquireSharedDirectory(relativeParent, actor);
    return paths.acquireFile(path.basename(absolute), actor, '').path;
  }
  function appendOutcome(row) {
    paths.continueFile(outcomeFile.path, outcomeActor, `${JSON.stringify(row)}\n`);
    return row;
  }
  function validateDynamicProjectInputs() {
    const runtimeRoot = path.resolve(descriptor.runtimeRoot || path.join(descriptor.root, 'runtime'));
    const inputs = descriptor.dynamicProjectInputs;
    if (!Array.isArray(inputs) || !inputs.length) {
      if (descriptor.dynamicManifestPath) throw new Error('Private dynamic project inputs are not bound');
      dynamicProjectInputs = new Map();
      dynamicProjectInputsValidated = true;
      return;
    }
    const checked = new Map();
    for (const input of inputs) {
      if (typeof input?.path !== 'string' || typeof input.sha256 !== 'string' || !/^[a-f0-9]{64}$/.test(input.sha256))
        throw new Error('Private dynamic project input identity is malformed');
      const absolute = path.resolve(runtimeRoot, input.path);
      if (!absolute.startsWith(`${runtimeRoot}${path.sep}`) || digestFile(absolute) !== input.sha256)
        throw new Error(`Private dynamic project input changed: ${input.path}`);
      checked.set(absolute, input.sha256);
    }
    if (checked.get(path.resolve(descriptor.configPath)) !== descriptor.configSHA256)
      throw new Error('Private dynamic project inputs omit the bound config');
    dynamicProjectInputs = checked;
    dynamicProjectInputsValidated = true;
  }
  function validateConfig(input) {
    if (route) throw new Error('Private config was validated twice');
    validateDynamicProjectInputs();
    route = validateOriginalOptions({ ...input, descriptor });
    for (const id of route.projectIds) {
      const output = prepareProject(id);
      route.projectOutputs[id] = output;
      route.projectPins[id] = projectPins[id];
    }
    configValidated = true;
    return route;
  }
  function routeProjects(projects, overrides) {
    const original = overrides?.__herdrC2Route;
    if (!original || !route || original !== route)
      throw new Error('Private route validation did not precede project construction');
    const ids = projects.map(project => cleanId(project.project.name));
    if (!ids.length || new Set(ids).size !== ids.length || ids.some(id => !allowedProjectNames.has(id)))
      throw new Error('Private selected project identities differ from the validated pair');
    for (const project of projects) {
      const id = cleanId(project.project.name);
      const outputDir = route.projectOutputs[id];
      if (!outputDir) throw new Error(`Selected project has no original validated output route: ${id}`);
      project.project.outputDir = outputDir;
    }
    route.projectIds = Object.keys(route.projectPins);
    assertRoute(route, descriptor);
    if (descriptor.routeManifestPath) {
      const entries = paths.snapshot().entries.map(item => item.path === 'control/outcomes.ndjson' ? { ...item, hash: undefined } : item);
      const manifest = { schema: 1, invocationId: descriptor.invocationId, root: paths.root, uid: paths.uid, route, entries };
      writeRetainedFile(descriptor.routeManifestPath, JSON.stringify(manifest, null, 2) + '\n', `route-manifest:${descriptor.invocationId}`);
    }
    return route;
  }
  function validateSerializedRoute(overrides) {
    const serialized = overrides?.__herdrC2Route;
    if (!serialized) throw new Error('Worker config omitted its private route');
    route = assertRoute(serialized, descriptor);
    for (const id of route.projectIds) projectPins[id] = route.projectPins[id];
    configValidated = true;
    return route;
  }
  function projectDirectory(projectId) {
    return outputFor(projectId);
  }
  function admitWorker(params) {
    const projectId = cleanId(params?.projectId);
    const workerIndex = Number(params?.workerIndex);
    if (!Number.isSafeInteger(workerIndex) || workerIndex < 0)
      throw new Error('Private worker index is invalid');
    const key = `${projectId}:${workerIndex}`;
    if (admittedWorkers.has(key)) throw new Error(`Private worker index was reused: ${key}`);
    const outputDir = outputFor(projectId);
    const artifactsDir = path.join(outputDir, `.playwright-artifacts-${workerIndex}`);
    if (params.artifactsDir && path.resolve(params.artifactsDir) !== artifactsDir)
      throw new Error('Worker artifact path differs from its mapped project route');
    const actor = `worker:${key}`;
    paths.registerActor(actor, outputDir);
    const admitted = paths.acquireSharedDirectory(`.playwright-artifacts-${workerIndex}`, actor);
    admittedWorkers.set(key, { actor, outputDir, artifactsDir: admitted.path, workerIndex, projectId });
    appendOutcome({ type: 'worker-admitted', projectId, workerIndex, path: admitted.path });
    return admitted.path;
  }
  function retainWorker(params) {
    const projectId = cleanId(params?.projectId);
    const workerIndex = Number(params?.workerIndex);
    const entry = admittedWorkers.get(`${projectId}:${workerIndex}`);
    if (!entry) throw new Error('Unadmitted worker retention refused');
    paths.retainPath(entry.artifactsDir, entry.actor);
    appendOutcome({ type: 'worker-retained', projectId, workerIndex, path: entry.artifactsDir });
    return entry.artifactsDir;
  }
  function admitTestOutput(testInfo, outputPath = testInfo.outputDir) {
    const projectId = cleanId(testInfo?.project?.name || testInfo?.project?.id);
    const outputDir = outputFor(projectId);
    const testRoot = path.resolve(testInfo.outputDir);
    const target = path.resolve(outputPath);
    if (!testRoot.startsWith(`${outputDir}${path.sep}`) || target !== testRoot && !target.startsWith(`${testRoot}${path.sep}`))
      throw new Error('Test output path escaped its mapped project/retry route');
    const testActor = `test:${testInfo.testId}:${testInfo.retry || 0}:${testInfo.repeatEachIndex || 0}`;
    const prior = admittedTestOutputs.get(testActor);
    if (prior && prior !== testRoot) throw new Error('Test output identity changed within a retry');
    if (!prior) admittedTestOutputs.set(testActor, testRoot);
    const existing = paths.objects.get(testRoot);
    if (existing) {
      if (existing.kind !== 'directory') throw new Error('Test output root is not an admitted directory');
      paths.retainPath(testRoot, existing.actor);
    } else {
      const relativeRoot = path.relative(outputDir, testRoot);
      const actor = `test-root:${crypto.createHash('sha256').update(testActor).digest('hex')}`;
      paths.registerActor(actor, outputDir);
      paths.acquireSharedDirectory(relativeRoot, actor);
      testOrdinal += 1;
      appendOutcome({ type: 'test-output-admitted', projectId, testId: String(testInfo.testId), retry: testInfo.retry || 0, repeatEachIndex: testInfo.repeatEachIndex || 0, ordinal: testOrdinal, path: testRoot });
    }
    return target;
  }
  function persistentProfile(inputPath, testInfo) {
    const output = admitTestOutput(testInfo);
    const supplied = path.resolve(inputPath);
    const expectedRoot = path.join(output, 'profiles');
    if (supplied !== expectedRoot && !supplied.startsWith(`${expectedRoot}${path.sep}`))
      throw new Error('Persistent browser profile is outside its admitted test output');
    const actor = `profile:${testInfo.testId}:${testInfo.retry || 0}:${profileOrdinal}`;
    if (launchedProfiles.has(actor)) throw new Error('Persistent profile admission was repeated');
    paths.registerActor(actor, output);
    const relative = path.relative(output, supplied);
    const lease = paths.acquireSharedDirectory(relative, actor);
    launchedProfiles.add(actor);
    profileOrdinal += 1;
    return lease.path;
  }
  function browserArtifacts(explicitPath) {
    const ordinal = artifactOrdinal++;
    const supplied = path.resolve(explicitPath || '');
    const worker = [...admittedWorkers.values()].find(entry => entry.artifactsDir === supplied);
    if (!worker) throw new Error('Browser artifact root was not admitted by a worker');
    const actor = `browser-artifacts:${worker.projectId}:${worker.workerIndex}:${ordinal}`;
    paths.registerActor(actor, worker.artifactsDir);
    const lease = paths.acquireSharedDirectory(`launch-${ordinal}`, actor);
    launchDirectories.push({ kind: 'artifacts', ordinal, path: lease.path });
    return lease.path;
  }
  function browserProfile(explicitPath, isPersistent) {
    if (isPersistent) {
      const supplied = path.resolve(explicitPath || '');
      const test = [...admittedTestOutputs].find(([, output]) => supplied.startsWith(`${output}${path.sep}`));
      if (!test) throw new Error('Persistent browser profile is outside an admitted test output');
      const [testActor, output] = test;
      const actor = `profile:${testActor}:${profileOrdinal++}`;
      paths.registerActor(actor, output);
      const relative = path.relative(output, supplied);
      const lease = paths.acquireSharedDirectory(relative, actor);
      launchedProfiles.add(actor);
      launchDirectories.push({ kind: 'persistent-profile', ordinal: profileOrdinal - 1, path: lease.path });
      return lease.path;
    }
    const ordinal = profileOrdinal++;
    const actor = `browser-profile:${ordinal}`;
    paths.registerActor(actor, fixedDomains.profiles);
    const lease = paths.acquireSharedDirectory(String(ordinal), actor);
    launchDirectories.push({ kind: 'profile', ordinal, path: lease.path });
    return lease.path;
  }
  function traceDirectory(actorName) {
    const safeActor = cleanId(actorName);
    const actor = `trace:${safeActor}`;
    const prior = admittedTraces.get(actor);
    if (prior) {
      paths.retainPath(prior, actor);
      return prior;
    }
    paths.registerActor(actor, fixedDomains.traces);
    const created = paths.acquireSharedDirectory(safeActor, actor).path;
    admittedTraces.set(actor, created);
    return created;
  }
  function temporaryPath(domainName, label) {
    const safeLabel = cleanId(label);
    if (!['uploads', 'downloads', 'reports', 'logs', 'control'].includes(domainName))
      throw new Error(`Unsupported private writer domain: ${domainName}`);
    const base = fixedDomains[domainName];
    const actor = `writer:${domainName}:${safeLabel}`;
    const prior = admittedTemporaryPaths.get(actor);
    if (prior) {
      paths.retainPath(prior, actor);
      return prior;
    }
    paths.registerActor(actor, base);
    const created = paths.acquireSharedDirectory(safeLabel, actor).path;
    admittedTemporaryPaths.set(actor, created);
    return created;
  }
  function lastRunFile(projectId) {
    return path.join(outputFor(projectId), '.last-run.json');
  }
  function retainTraceDirectory(target) {
    const entry = [...admittedTraces].find(([, value]) => value === path.resolve(target));
    if (!entry) throw new Error('Fallback trace directory was not admitted');
    const actor = entry[0];
    return paths.retainPath(target, actor);
  }
  function admitArtifactIntent(target) {
    const absolute = path.resolve(target);
    const existing = paths.objects.get(absolute);
    if (existing) {
      if (existing.kind !== 'file') throw new Error('Artifact target is not a private file');
      return existing.actor;
    }
    if (!absolute.startsWith(`${paths.root}${path.sep}`) || fs.existsSync(absolute))
      throw new Error(`Artifact path is outside its private invocation or already exists: ${absolute}`);
    const parentPath = path.dirname(absolute);
    const base = [...paths.objects.entries()]
      .filter(([candidate, lease]) => lease.kind === 'directory' && (parentPath === candidate || parentPath.startsWith(`${candidate}${path.sep}`)))
      .sort((left, right) => right[0].length - left[0].length)[0]?.[0];
    if (!base) throw new Error(`Artifact parent has no enrolled writer domain: ${parentPath}`);
    const actor = `artifact:${crypto.randomUUID()}`;
    paths.registerActor(actor, base);
    const relativeParent = path.relative(base, parentPath);
    if (relativeParent) paths.acquireSharedDirectory(relativeParent, actor);
    const parent = paths.actorBase(actor);
    const filePath = path.join(parent, path.basename(absolute));
    if (filePath !== absolute || fs.existsSync(filePath)) throw new Error('Artifact path admission collided or changed');
    artifactIntents.set(absolute, actor);
    appendOutcome({ type: 'artifact-intent', path: absolute, actor });
    return actor;
  }
  function completeArtifactIntent(target) {
    const absolute = path.resolve(target);
    const actor = artifactIntents.get(absolute);
    if (!actor) {
      const existing = paths.objects.get(absolute);
      if (existing?.kind === 'file') return paths.retainPath(absolute, existing.actor);
      throw new Error(`Artifact completion has no prior writer intent: ${absolute}`);
    }
    if (!fs.existsSync(absolute)) {
      appendOutcome({ type: 'artifact-incomplete', path: absolute, actor });
      return { pending: absolute, actor };
    }
    const admitted = paths.admitProducedFile(absolute, actor);
    appendOutcome({ type: 'artifact-produced', path: absolute, actor, dev: admitted.identity.dev, ino: admitted.identity.ino });
    return admitted;
  }
  function retainAdmittedPath(target) {
    const absolute = path.resolve(target);
    let entry = paths.objects.get(absolute);
    if (!entry && artifactIntents.has(absolute)) {
      if (!fs.existsSync(absolute)) return { pending: absolute, actor: artifactIntents.get(absolute) };
      completeArtifactIntent(absolute);
      entry = paths.objects.get(absolute);
    }
    if (!entry) throw new Error(`Private path was not admitted before writing: ${absolute}`);
    return paths.retainPath(absolute, entry.actor);
  }
  function relocateAdmittedFile(source, destination) {
    const absolute = path.resolve(source);
    const entry = paths.objects.get(absolute);
    if (!entry || entry.kind !== 'file') throw new Error('Private relocation source was not admitted');
    return paths.relocate(absolute, destination, entry.actor);
  }
  function retainCacheDirectory(cachePath) {
    const absolute = path.resolve(cachePath);
    const cacheRoot = fixedDomains['transform-cache'];
    if (absolute !== cacheRoot && !absolute.startsWith(`${cacheRoot}${path.sep}`))
      throw new Error('Transform-cache invalidation escaped its private root');
    const entry = paths.objects.get(absolute);
    if (!entry) {
      if (fs.existsSync(absolute)) throw new Error('Transform-cache path exists without a private admission');
      return false;
    }
    return paths.retainPath(absolute, entry.actor);
  }
  function verifyWorkerAdmission(params) {
    const key = `${cleanId(params?.projectId)}:${Number(params?.workerIndex)}`;
    const entry = admittedWorkers.get(key);
    if (!entry || path.resolve(params.artifactsDir) !== entry.artifactsDir)
      throw new Error('Worker artifact path was not admitted before fork');
    return paths.retainPath(entry.artifactsDir, entry.actor);
  }
  function retainTestOutput(testInfo) {
    const actor = `test:${testInfo.testId}:${testInfo.retry || 0}:${testInfo.repeatEachIndex || 0}`;
    const output = admittedTestOutputs.get(actor);
    if (!output) throw new Error('Test output was not admitted before its finalizer');
    const lease = paths.objects.get(output);
    if (!lease || lease.kind !== 'directory') throw new Error('Test output root identity was not enrolled');
    return paths.retainPath(output, lease.actor);
  }
  function admitUploadRoot(tmpDir, rootDirName) {
    const parentActor = [...admittedTemporaryPaths].find(([, value]) => value === tmpDir)?.[0];
    if (!parentActor) throw new Error('Upload temporary root was not admitted');
    if (!rootDirName) {
      admittedUploadRoots.set(tmpDir, parentActor);
      return tmpDir;
    }
    const name = path.basename(String(rootDirName));
    cleanId(name);
    const childActor = `upload-root:${name}:${crypto.randomUUID()}`;
    paths.registerActor(childActor, tmpDir);
    const created = paths.acquireSharedDirectory(name, childActor).path;
    admittedUploadRoots.set(created, childActor);
    return created;
  }
  function admitUploadFile(uploadRoot, itemName, label) {
    const parentActor = admittedUploadRoots.get(path.resolve(uploadRoot));
    if (!parentActor) throw new Error('Upload file root was not admitted');
    const actor = `upload-file:${cleanId(label)}`;
    paths.registerActor(actor, path.resolve(uploadRoot));
    const parts = String(itemName).split(/[\\/]+/);
    if (parts.some(part => !part || part === '.' || part === '..')) throw new Error('Upload file path is unsafe');
    return paths.acquireFile(parts.join('/'), actor, '').path;
  }
  function admitUploadDirectory(label) {
    return temporaryPath('uploads', `upload-${label}`);
  }
  function admitDownloadDirectory(label) {
    return temporaryPath('downloads', `download-${label}`);
  }
  function verifyProjectOutput(projectId) {
    const output = outputFor(projectId);
    const stat = fs.lstatSync(output);
    const expected = projectPins[cleanId(projectId)];
    if (!stat.isDirectory() || stat.isSymbolicLink() || Number(stat.uid) !== paths.uid || String(stat.dev) !== expected.dev || String(stat.ino) !== expected.ino || fs.realpathSync(output) !== output)
      throw paths.fail(new Error(`Private project output identity changed: ${projectId}`));
    return output;
  }
  return {
    descriptor,
    paths,
    validateConfig,
    validateDynamicProjectInputs,
    routeProjects,
    validateSerializedRoute,
    projectDirectory,
    outputFor,
    admitWorker,
    retainWorker,
    admitTestOutput,
    persistentProfile,
    browserArtifacts,
    browserProfile,
    traceDirectory,
    temporaryPath,
    lastRunFile,
    verifyProjectOutput,
    appendOutcome,
    writeRetainedFile,
    writeCacheFile,
    admitWritableFile,
    retainTraceDirectory,
    retainAdmittedPath,
    admitArtifactIntent,
    completeArtifactIntent,
    relocateAdmittedFile,
    retainCacheDirectory,
    verifyWorkerAdmission,
    retainTestOutput,
    admitUploadRoot,
    admitUploadFile,
    admitUploadDirectory,
    admitDownloadDirectory,
    cacheWritesAllowed: () => configValidated,
    dynamicProjectInputs: () => {
      if (!dynamicProjectInputsValidated) throw new Error('Dynamic project inputs are unavailable before input validation');
      return new Map(dynamicProjectInputs);
    },
    retainPath: (target, actor) => paths.retainPath(target, actor),
    relocate: (source, destination, actor) => paths.relocate(source, destination, actor),
    transformCacheDir: () => fixedDomains['transform-cache'],
    assertExecutionReady: () => { throw new Error('C2 private execution remains closed pending C3/C4 readiness'); },
    nativeEffectCount: () => 0,
    snapshot: () => ({ paths: paths.snapshot(), projectPins, launchDirectories, workerCount: admittedWorkers.size, profileCount: profileOrdinal, artifactCount: artifactOrdinal })
  };
}

module.exports = { createPrivateRoute, validateOriginalOptions, assertRoute, inspectPathOptions, assertSafeArguments, validateReporter, effectiveOutput, allowedProjectNames };
