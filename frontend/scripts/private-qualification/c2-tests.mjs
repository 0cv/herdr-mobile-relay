import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { createRequire, Module } from 'node:module';
import { createHash } from 'node:crypto';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { applyC2Edits, justifiedDynamicSiteExpansions } from './c2-edits.mjs';
import { assertC2BudgetEnvelope, assertDynamicSiteScanClosed, assertGeneratedDynamicScanClosed, assertOriginalDynamicSiteScanClosed, assertOriginalDynamicSiteSemanticsUnambiguous, scanDynamicSites } from './c2-generate.mjs';

const cases = [
  ['C2-01', ['RES01-before-require']],
  ['C2-02', ['CJS-entry', 'ESM-entry', 'static-private-edges', 'dynamic-refusal', 'stock-resolution-refusal']],
  ['C2-03', ['canonical-bootstrap-support', 'runner-consumer', 'worker-consumer', 'duplicate-config-refusal']],
  ['C2-04', ['CLI-project-root-default-precedence', 'both-projects-before-reporters', 'Chromium-and-WebKit-routing']],
  ['C2-05', ['unselected-project-output-refusal', 'cache-override-refusal', 'profile-override-refusal', 'custom-reporter-refusal', 'custom-hook-refusal', 'electron-browser-refusal', 'UI-refusal', 'watch-refusal', 'proxy-refusal']],
  ['C2-06', ['serialize-roundtrip', 'missing-route-refusal', 'tampered-route-refusal', 'mixed-invocation-refusal']],
  ['C2-07', ['runtime-project-control-log-report-positive', 'same-actor-continuation', 'retained-identities']],
  ['C2-08', ['EEXIST-collision', 'symlink', 'nonregular', 'hardlink', 'UID-permission-refusal']],
  ['C2-09', ['parent-replacement', 'object-replacement', 'actor-rebind', 'inspection-error', 'ENOSPC-refusal']],
  ['C2-10', ['C1-worker-admission-callback', 'unique-workerIndex', 'refusal-before-fork', 'worker-retention']],
  ['C2-11', ['first-output', 'outputPath', 'retry', 'repeatEach', 'attachment-route', 'no-pre-or-final-delete']],
  ['C2-12', ['code-cache-hash', 'map-data-cache', 'no-cache-sweep', 'deferred-until-config-validation']],
  ['C2-13', ['clear-output-root-verification', 'EBUSY-no-child-delete']],
  ['C2-14', ['automatic-profile', 'explicit-profile', 'artifact-launch-ordinal', 'failed-preparation-retained']],
  ['C2-15', ['raw-trace', 'stack-trace', 'Artifact-completion', 'passing-discard-no-attachment']],
  ['C2-16', ['single-source-relocation', 'destination-collision', 'multi-source-retention', 'partial-retention']],
  ['C2-17', ['upload-file', 'download-artifact', 'LastRun', 'unsupported-overrides']],
  ['C2-18', ['private-release-fixture', 'profile-adapter', 'assertions-preserved', 'close-semantics']],
  ['C2-19', ['private-before-effects', 'missing-prerequisite-refusal', 'no-stock-fallthrough', 'stock-contract-preserved']],
  ['C2-20', ['reused-C1-identity', 'native-guard-closed', 'distinct-invocation-receipts', 'historical-counters-preserved']]
];
export const expectedC2CaseVariants = cases.map(([id, variants]) => ({ id, variants }));

function hash(value) {
  return createHash('sha256').update(value).digest('hex');
}

function sha1(value) {
  return createHash('sha1').update(value).digest('hex');
}

function sourceBinding(context, file, needle) {
  const source = context.generated[file] ?? context.projectSources[file] ?? (file === 'frontend/scripts/run-browser-tests.sh' ? context.wrapperSource : undefined);
  if (typeof source !== 'string') throw new Error(`C2 source file is not bound: ${file}`);
  const index = source.indexOf(needle);
  if (index < 0) throw new Error(`C2 source binding was not found: ${file}:${needle}`);
  return {
    file,
    line: source.slice(0, index).split('\n').length,
    byteStart: Buffer.byteLength(source.slice(0, index)),
    bytes: Buffer.byteLength(needle),
    generatedSHA256: hash(source)
  };
}

function evaluateActualLine(line, context) {
  return vm.runInNewContext(line, context);
}

function plainConfig(descriptor, packageJsonDir, changes = {}) {
  const project = (name, use) => ({ name, use });
  const webRoot = descriptor.webRoot;
  return {
    location: { resolvedConfigFile: descriptor.configPath, configDir: packageJsonDir },
    userConfig: {
      testDir: './tests/browser',
      timeout: 30000,
      expect: { timeout: 5000 },
      fullyParallel: true,
      retries: 0,
      workers: undefined,
      use: { baseURL: 'http://127.0.0.1:4173', serviceWorkers: 'block', trace: 'retain-on-failure' },
      projects: [
        project('chromium-mobile', { defaultBrowserType: 'chromium', deviceScaleFactor: 2.625, isMobile: true, viewport: { width: 412, height: 915 } }),
        project('webkit-mobile', { defaultBrowserType: 'webkit', deviceScaleFactor: 3, isMobile: true, viewport: { width: 393, height: 852 } })
      ],
      webServer: { command: `node scripts/browser-server.mjs ${JSON.stringify(webRoot)}`, port: 4173, reuseExistingServer: false },
      ...changes.userConfig
    },
    configCLIOverrides: { argv: [], ...changes.overrides },
    packageJsonDir,
    argv: [],
    env: { HERDR_WEB_ROOT: webRoot, ...changes.env }
  };
}

function loadIsolatedCjs(filename, cache = new Map()) {
  const absolute = path.resolve(filename);
  if (cache.has(absolute)) return cache.get(absolute);
  const source = fs.readFileSync(absolute, 'utf8');
  const localRequire = specifier => {
    if (specifier.startsWith('.')) {
      const resolved = createRequire(absolute).resolve(specifier);
      if (resolved.endsWith('.cjs')) return loadIsolatedCjs(resolved, cache);
    }
    return createRequire(absolute)(specifier);
  };
  const moduleRecord = { exports: {}, require: localRequire };
  const wrapper = vm.runInThisContext(`(function(module, exports) { ${source}\n})`, { filename: absolute });
  wrapper(moduleRecord, moduleRecord.exports);
  cache.set(absolute, moduleRecord.exports);
  return moduleRecord.exports;
}

function createCaseScope(context, id) {
  const { paths, pathTools } = context;
  const casesRoot = context.casesRoot;
  const token = crypto.randomUUID();
  const actor = `case-${id}-${token}`;
  paths.registerActor(actor, casesRoot);
  const caseRoot = paths.acquireSharedDirectory(`${id}-${token}`, actor).path;
  const stageIdentity = pathTools.identity(fs.lstatSync(caseRoot));
  const childStage = {
    path: caseRoot,
    parent: path.dirname(caseRoot),
    parentAncestry: pathTools.pinExistingAncestry(path.dirname(caseRoot)),
    identity: stageIdentity,
    uid: context.paths.uid
  };
  const invocationId = crypto.randomUUID();
  const invocationRoot = path.join(caseRoot, `invocation-${invocationId}`);
  const child = pathTools.createInvocation(childStage, invocationRoot, invocationId);
  context.childScopes.push({ root: invocationRoot, id: invocationId, pathPolicy: child });
  return child;
}

function makeCaseConfig(context, caseId, source = context.projectSources['frontend/playwright.config.ts']) {
  const child = createCaseScope(context, caseId);
  const configPath = path.join(child.root, 'playwright.config.ts');
  child.acquireFile('playwright.config.ts', 'controller', source);
  const descriptor = {
    schema: 1,
    invocationId: child.id,
    root: child.root,
    uid: child.uid,
    nativeReady: false,
    configPath,
    configSHA256: hash(source),
    webRoot: context.descriptor.webRoot,
    deadline: Number.MAX_SAFE_INTEGER
  };
  const route = context.routeFactory(child, descriptor);
  return { child, descriptor, route, input: plainConfig(descriptor, path.dirname(configPath)) };
}

function extractBalancedCall(source, start) {
  const open = source.indexOf('(', start);
  let depth = 0;
  let quote = '';
  let escaped = false;
  for (let index = open; index < source.length; index += 1) {
    const character = source[index];
    if (quote) {
      if (escaped) escaped = false;
      else if (character === '\\') escaped = true;
      else if (character === quote) quote = '';
      continue;
    }
    if (character === '\'' || character === '"' || character === '`') { quote = character; continue; }
    if (character === '(') depth += 1;
    if (character === ')' && --depth === 0) return source.slice(start, index + 1);
  }
  throw new Error('C2 generated dynamic guard call is incomplete');
}

function extractGeneratedFunction(source, marker, endMarker) {
  const start = source.indexOf(marker);
  const end = source.indexOf(endMarker, start);
  if (start < 0 || end < 0) throw new Error(`C2 generated function boundary is missing: ${marker}`);
  return source.slice(start, end).trimEnd();
}

function replaceThirdArgument(call) {
  const open = call.indexOf('(');
  const commas = [];
  let parentheses = 1;
  let brackets = 0;
  let braces = 0;
  let quote = '';
  let escaped = false;
  for (let index = open + 1; index < call.length; index += 1) {
    const character = call[index];
    if (quote) {
      if (escaped) escaped = false;
      else if (character === '\\') escaped = true;
      else if (character === quote) quote = '';
      continue;
    }
    if (character === '\'' || character === '"' || character === '`') { quote = character; continue; }
    if (character === '(') parentheses += 1;
    else if (character === ')') {
      parentheses -= 1;
      if (parentheses === 0) break;
    } else if (character === '[') brackets += 1;
    else if (character === ']') brackets -= 1;
    else if (character === '{') braces += 1;
    else if (character === '}') braces -= 1;
    else if (character === ',' && parentheses === 1 && brackets === 0 && braces === 0) commas.push(index);
  }
  if (commas.length < 3) throw new Error('C2 generated dynamic guard does not have four arguments');
  return `${call.slice(0, commas[1] + 1)} () => specifier${call.slice(commas[2])}`;
}

function extractGeneratedHelper(source, marker) {
  const start = source.indexOf(marker);
  const open = source.indexOf('{', start);
  if (start < 0 || open < 0) throw new Error(`C2 generated helper is missing: ${marker}`);
  let depth = 0;
  let quote = '';
  let escaped = false;
  for (let index = open; index < source.length; index += 1) {
    const character = source[index];
    if (quote) {
      if (escaped) escaped = false;
      else if (character === '\\') escaped = true;
      else if (character === quote) quote = '';
      continue;
    }
    if (character === '\'' || character === '"' || character === '`') { quote = character; continue; }
    if (character === '{') depth += 1;
    if (character === '}' && --depth === 0) return source.slice(start, index + 1);
  }
  throw new Error(`C2 generated helper is incomplete: ${marker}`);
}

function runUntilClosingBrace(source, start, marker) {
  const at = source.indexOf(marker, start);
  if (at < 0) throw new Error(`C2 generated source marker is missing: ${marker}`);
  const open = source.indexOf('{', at);
  let depth = 0;
  let quote = '';
  let escaped = false;
  for (let index = open; index < source.length; index += 1) {
    const character = source[index];
    if (quote) {
      if (escaped) escaped = false;
      else if (character === '\\') escaped = true;
      else if (character === quote) quote = '';
      continue;
    }
    if (character === '"' || character === "'" || character === '`') {
      quote = character;
      continue;
    }
    if (character === '{') depth += 1;
    if (character === '}' && --depth === 0) return source.slice(at, index + 1);
  }
  throw new Error(`C2 generated source function is incomplete: ${marker}`);
}

export async function runC2GHarnessRegressions() {
  const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../..');
  const baselinePath = '/private/tmp/fullscreen-c2-stage-plan-20261006-0004/baseline.json';
  const inventoryPath = '/private/tmp/fullscreen-c2-dynamic-repair-20261006/evidence/dynamic-site-inventory.json';
  const baselineBytes = fs.readFileSync(baselinePath);
  assert.equal(hash(baselineBytes), 'eb3aa68403869d3c9565be4dc3985910d7c3764d189556a2b5da82174b6b6db3');
  const baseline = JSON.parse(baselineBytes.toString('utf8'));
  const inventoryBytes = fs.readFileSync(inventoryPath);
  assert.equal(hash(inventoryBytes), '39b5647cfc555097e1738b411e9b13b946ae0488dca09dfd45782af8da2b895f');
  const inventory = JSON.parse(inventoryBytes.toString('utf8'));
  const sourcePaths = [
    'frontend/scripts/run-browser-tests.sh',
    'frontend/scripts/private-qualification/c2-generate.mjs',
    'frontend/scripts/private-qualification/c2-edits.mjs',
    'frontend/scripts/private-qualification/c2-bootstrap.cjs',
    'frontend/scripts/private-qualification/c2-paths.cjs',
    'frontend/scripts/private-qualification/c2-route.cjs',
    'frontend/scripts/private-qualification/c2-tests.mjs',
    'frontend/scripts/private-qualification/c2-check.mjs'
  ];
  const sourceHashesBefore = Object.fromEntries(sourcePaths.map(sourcePath => [sourcePath, hash(fs.readFileSync(path.join(repositoryRoot, sourcePath)))]));
  const closureSources = {};
  const originalSources = {};
  for (const input of baseline.closureCandidate.filter(item => /\.(?:[cm]?js|ts)$/.test(item.path) && !item.path.endsWith('.d.ts') && !item.path.includes('/node_modules/playwright/node_modules/fsevents/'))) {
    const absolute = path.join(repositoryRoot, input.path);
    const stat = fs.lstatSync(absolute);
    const bytes = fs.readFileSync(absolute);
    assert.ok(stat.isFile() && !stat.isSymbolicLink());
    assert.equal(stat.size, input.bytes);
    assert.equal(hash(bytes), input.sha256, `frozen source changed: ${input.path}`);
    closureSources[input.path] = bytes.toString('utf8');
    originalSources[input.path] = bytes;
  }
  const originalScanSites = Object.entries(closureSources).flatMap(([sourcePath, source]) => scanDynamicSites(sourcePath, source));
  const originalScan = assertOriginalDynamicSiteScanClosed(originalScanSites, inventory.sites, originalSources);
  assert.equal(originalScan.matchedSites, 37);
  assert.ok(originalScan.matches.every(match => match.occurrenceMapping.unambiguous));
  const projectSources = {};
  for (const input of baseline.inputs) {
    const absolute = path.join(repositoryRoot, input.path);
    const bytes = fs.readFileSync(absolute);
    const source = bytes.toString('utf8');
    if (input.path === 'frontend/scripts/run-browser-tests.sh') {
      const branchStart = source.indexOf('if [ "${HERDR_FULLSCREEN_C2_PRIVATE:-}" = 1 ]; then');
      const branchEnd = source.indexOf('\n\nSCRIPT_DIR=', branchStart);
      assert.ok(branchStart >= 0 && branchEnd > branchStart);
      assert.equal(hash(Buffer.from(source.slice(0, branchStart) + source.slice(branchEnd + 2))), input.sha256, 'stock browser wrapper changed outside its private branch');
    } else {
      assert.equal(bytes.length, input.bytes);
      assert.equal(hash(bytes), input.sha256, `frozen project input changed: ${input.path}`);
    }
    projectSources[input.path] = source;
  }
  const codePaths = new Set([
    'frontend/node_modules/@playwright/test/cli.js',
    'frontend/node_modules/playwright/lib/common/index.js',
    'frontend/node_modules/playwright/lib/runner/index.js',
    'frontend/node_modules/playwright/lib/worker/workerProcessEntry.js',
    'frontend/node_modules/playwright-core/lib/coreBundle.js',
    'frontend/node_modules/playwright/lib/transform/esmLoader.js',
    ...inventory.sites.map(site => site.from)
  ]);
  const sources = Object.fromEntries([...codePaths].map(sourcePath => {
    const source = closureSources[sourcePath];
    assert.equal(typeof source, 'string', `selected source missing: ${sourcePath}`);
    return [sourcePath, source];
  }));
  const dynamicEditSites = inventory.sites.map(site => {
    const match = originalScan.matches.find(item => item.siteId === site.siteId);
    return { ...site, originalOccurrence: match.originalCoordinate.occurrence, originalCoordinate: match.originalCoordinate, enforcedBy: site.siteId >= 'DS06' && site.siteId <= 'DS17' ? ['DS23', 'DS25'] : [] };
  });
  const edited = applyC2Edits({ sources, releaseFixture: projectSources['frontend/tests/browser/release-resources.spec.ts'], dynamicSites: dynamicEditSites });
  const generatedSources = { ...closureSources, ...projectSources, ...edited.generated };
  const dynamicSiteRecords = inventory.sites.map(site => {
    const source = generatedSources[site.from];
    const original = originalSources[site.from].toString('utf8');
    const guardPattern = new RegExp(`(?:privateC2\\.(?:dynamicModule|enforceDynamicSite)|__c2DynamicAsset)\\("${site.siteId}"`);
    const guardLine = source.split('\n').findIndex(line => guardPattern.test(line)) + 1;
    const generatedExpansions = justifiedDynamicSiteExpansions.filter(expansion => expansion.siteId === site.siteId).map(expansion => {
      const originalOffset = original.indexOf(expansion.originalText);
      const generatedOffset = source.indexOf(expansion.generatedText);
      assert.ok(originalOffset >= 0 && generatedOffset >= 0);
      return {
        expansionId: expansion.expansionId,
        kind: expansion.kind,
        generatedExpressionSHA256: hash(Buffer.from(expansion.expression)),
        originalCoordinate: { path: expansion.file, line: original.slice(0, originalOffset).split('\n').length, sourceOffset: originalOffset, representation: 'raw-original-source-expansion' },
        justification: expansion.justification
      };
    });
    assert.ok(guardLine > 0, `guard missing: ${site.siteId}`);
    return {
      ...site,
      kind: originalScan.matches.find(match => match.siteId === site.siteId).originalCoordinate.kind,
      originalCoordinate: originalScan.matches.find(match => match.siteId === site.siteId).originalCoordinate,
      generatedFile: site.from,
      generatedSHA256: hash(Buffer.from(source)),
      guardLine,
      generatedExpansions,
      enforcedBy: site.siteId >= 'DS06' && site.siteId <= 'DS17' ? ['DS23', 'DS25'] : []
    };
  });
  const generatedScanSites = Object.entries(generatedSources).flatMap(([sourcePath, source]) => /\.(?:[cm]?js|ts)$/.test(sourcePath) && !sourcePath.endsWith('.d.ts') ? scanDynamicSites(sourcePath, source) : []);
  const generatedScan = assertGeneratedDynamicScanClosed(generatedScanSites, dynamicSiteRecords, generatedSources);
  assert.equal(generatedScan.generatedGuards.filter(guard => guard.siteId === 'DS02').length, 3);
  assert.equal(generatedScan.generatedDelegatedCalls.length, 12);
  const ds02 = dynamicSiteRecords.find(site => site.siteId === 'DS02');
  assert.equal(ds02.generatedGuardOccurrences.filter(item => item.provenance.type === 'frozen-inventory-occurrence').length, 1);
  assert.deepEqual(ds02.generatedExpansions.map(item => item.expansionId).sort(), ['DS02-ESM-import', 'DS02-ESM-preflight']);
  assert.ok(ds02.generatedExpansions.every(item => item.generatedCoordinate?.line > 0 && item.originalCoordinate?.sourceOffset >= 0));
  const commonName = 'frontend/node_modules/playwright/lib/common/index.js';
  const commonSource = generatedSources[commonName];
  const commonPath = '/private/tmp/c2-G-mapping-regression/project/frontend/node_modules/playwright/lib/common/index.js';
  const caller = extractGeneratedFunction(commonSource, 'async function requireOrImport(file)', '\nvar transformInstalled =');
  const dynamicCaller = caller.replace(/\bimport\(/g, 'dynamicImport(');
  const configPath = '/private/tmp/c2-G-mapping-regression/project/frontend/playwright.config.mjs';
  const expectedUrl = pathToFileURL(configPath).href;
  const makeRuntime = ({ loaderChannel = false } = {}) => {
    const state = { decisions: [], required: [], imports: [], preflightErrors: [], nextTaskCalls: 0, effectsBeforeDecision: 0 };
    const privateC2 = {
      dynamicModule(siteId, kind, getSpecifier) {
        const specifier = getSpecifier();
        state.decisions.push({ siteId, kind, specifier });
        if (specifier === `${expectedUrl}.esm.preflight`) throw Object.assign(new Error('private preflight refused'), { name: 'C2DynamicModuleRefusalError', siteId });
        if (specifier !== configPath && specifier !== expectedUrl) throw Object.assign(new Error('private target refused'), { name: 'C2DynamicModuleRefusalError', siteId });
        return kind === 'import' ? expectedUrl : configPath;
      }
    };
    const requireOrImport = vm.runInNewContext(`${dynamicCaller}; requireOrImport`, {
      privateC2,
      __filename: commonPath,
      installTransformIfNeeded() {},
      fileIsModule: () => true,
      import_url2: { default: { pathToFileURL } },
      loaderChannel,
      nextTask() { state.nextTaskCalls += 1; },
      debugTest(...args) { state.preflightErrors.push(args.at(-1)); },
      dynamicImport(specifier) { state.imports.push(specifier); return Promise.resolve({ loaded: specifier }); },
      require(specifier) { state.required.push(specifier); return { loaded: specifier }; },
      currentFileDepsCollector: () => undefined
    });
    return { state, requireOrImport };
  };
  const cjs = makeRuntime();
  cjs.requireOrImport = vm.runInNewContext(`${caller}; requireOrImport`, {
    privateC2: { ...cjs.state, dynamicModule(siteId, kind, getSpecifier) { const specifier = getSpecifier(); cjs.state.decisions.push({ siteId, kind, specifier }); return specifier; } },
    __filename: commonPath,
    installTransformIfNeeded() {},
    fileIsModule: () => false,
    require(specifier) { cjs.state.required.push(specifier); return { loaded: specifier }; },
    currentFileDepsCollector: () => undefined
  });
  assert.deepEqual(await cjs.requireOrImport(configPath), { loaded: configPath });
  assert.deepEqual(cjs.state.required, [configPath]);
  assert.equal(cjs.state.decisions[0].siteId, 'DS02');
  const esm = makeRuntime();
  await esm.requireOrImport(configPath);
  assert.deepEqual(esm.state.imports, [expectedUrl]);
  assert.deepEqual(esm.state.decisions.map(item => [item.siteId, item.kind]), [['DS02', 'import']]);
  const loader = makeRuntime({ loaderChannel: true });
  await loader.requireOrImport(configPath);
  assert.deepEqual(loader.state.decisions.map(item => item.specifier), [`${expectedUrl}.esm.preflight`, expectedUrl]);
  assert.deepEqual(loader.state.imports, [expectedUrl]);
  assert.equal(loader.state.preflightErrors.length, 1);
  assert.equal(loader.state.preflightErrors[0].siteId, 'DS02');
  assert.equal(loader.state.nextTaskCalls, 2);
  const invocationLimits = { allocatedBytes: 64 * 1024 * 1024, entries: 8192, maxDepth: 32 };
  const invocationReceipt = assertC2BudgetEnvelope({ allocatedBytes: invocationLimits.allocatedBytes, logicalBytes: 128 * 1024 * 1024, entries: invocationLimits.entries, maxDepth: invocationLimits.maxDepth }, invocationLimits, 'G invocation regression');
  assert.equal(invocationReceipt.basis, 'allocatedBytes');
  assert.equal(invocationReceipt.allocatedBytes, invocationLimits.allocatedBytes);
  assert.throws(() => assertC2BudgetEnvelope({ allocatedBytes: invocationLimits.allocatedBytes + 1, entries: 1, maxDepth: 1 }, invocationLimits), /effective output envelope/);
  assert.throws(() => assertC2BudgetEnvelope({ allocatedBytes: 1, entries: 8193, maxDepth: 1 }, invocationLimits), /effective output envelope/);
  assert.throws(() => assertC2BudgetEnvelope({ allocatedBytes: 1, entries: 1, maxDepth: 33 }, invocationLimits), /effective output envelope/);
  const stageEnvelope = assertC2BudgetEnvelope({ allocatedBytes: 80 * 1024 * 1024 }, { allocatedBytes: 80 * 1024 * 1024 }, 'G stage regression');
  const campaignEnvelope = assertC2BudgetEnvelope({ allocatedBytes: 640 * 1024 * 1024 }, { allocatedBytes: 640 * 1024 * 1024 }, 'G campaign regression');
  assert.equal(stageEnvelope.basis, 'allocatedBytes');
  assert.equal(campaignEnvelope.basis, 'allocatedBytes');
  const checkSource = fs.readFileSync(path.join(repositoryRoot, 'frontend/scripts/private-qualification/c2-check.mjs'), 'utf8');
  assert.match(checkSource, /function estimateScope\([\s\S]*?assertC2BudgetEnvelope\([\s\S]*?'projected invocation'/);
  assert.match(checkSource, /'G1 preflight invocation forecast'/);
  assert.match(checkSource, /'G1 retained invocation'/);
  assert.match(checkSource, /'G1 invocation receipt'/);
  const sourceHashesAfter = Object.fromEntries(sourcePaths.map(sourcePath => [sourcePath, hash(fs.readFileSync(path.join(repositoryRoot, sourcePath)))]));
  assert.deepEqual(sourceHashesAfter, sourceHashesBefore);
  return {
    status: 'passed',
    execution: 'harness-only',
    regressionExecutions: 1,
    inventorySHA256: hash(inventoryBytes),
    sourceHashes: sourceHashesAfter,
    mapping: { status: 'passed', originalOccurrences: originalScan.matchedSites, generatedOccurrences: generatedScan.scannedSites, guardedOccurrences: generatedScan.matchedSites, delegatedOccurrences: generatedScan.generatedDelegatedCalls.length },
    ds02: { status: 'passed', guardedWrappers: generatedScan.generatedGuards.filter(guard => guard.siteId === 'DS02').length, expansionIds: ds02.generatedExpansions.map(item => item.expansionId).sort(), requireCalls: cjs.state.decisions.length, esmCalls: esm.state.decisions.length, loaderChannelCalls: loader.state.decisions.length, preflightRefusals: loader.state.preflightErrors.length, loaderImports: loader.state.imports.length },
    budget: { status: 'passed', basis: 'allocatedBytes', invocationBytes: invocationLimits.allocatedBytes, entries: invocationLimits.entries, depth: invocationLimits.maxDepth, stageBytes: stageEnvelope.allocatedBytes, campaignBytes: campaignEnvelope.allocatedBytes, logicalBytesIgnoredByInvocationEnvelope: true }
  };
}

export async function runC2Tests(context) {
  const [shimName, commonName, runnerName, workerName, coreName, fixtureName] = [
    'frontend/node_modules/@playwright/test/cli.js',
    'frontend/node_modules/playwright/lib/common/index.js',
    'frontend/node_modules/playwright/lib/runner/index.js',
    'frontend/node_modules/playwright/lib/worker/workerProcessEntry.js',
    'frontend/node_modules/playwright-core/lib/coreBundle.js',
    'frontend/tests/browser/release-resources.spec.ts'
  ];
  const common = context.generated[commonName];
  const runner = context.generated[runnerName];
  const worker = context.generated[workerName];
  const core = context.generated[coreName];
  const fixture = context.generated[fixtureName];
  const runtime = context.bootstrap.getRuntime();
  const route = runtime.route;
  const results = [];
  const sourceRefs = {
    shim: sourceBinding(context, shimName, 'privateC2.beforeProgramLoad();'),
    config: sourceBinding(context, commonName, 'privateC2.getRuntime().route.validateConfig('),
    projects: sourceBinding(context, commonName, 'privateC2.getRuntime().route.routeProjects('),
    runner: sourceBinding(context, runnerName, 'privateC2.getRuntime().route.verifyProjectOutput('),
    worker: sourceBinding(context, workerName, 'privateC2.getRuntime().route.admitWritableFile('),
    workerGuard: sourceBinding(context, workerName, 'privateC2.beforeWorkerLoad();'),
    core: sourceBinding(context, coreName, 'route.browserArtifacts(options.artifactsDir)'),
    fixture: sourceBinding(context, fixtureName, "test('normal HTTP caching revalidates unchanged bytes and replaces a repaired wrong response")
  };
  const snippetValidation = common.split('\n').find(line => line.includes('privateC2.getRuntime().route.validateConfig('));
  const snippetProjects = common.split('\n').find(line => line.includes('privateC2.getRuntime().route.routeProjects('));
  const deserializeLine = common.split('\n').find(line => line.includes('privateC2.getRuntime().route.validateSerializedRoute(data.configCLIOverrides)'));
  const serializeLine = common.split('\n').find(line => line.includes('privateC2.getRuntime().route.validateSerializedRoute(config.configCLIOverrides)'));
  if (!snippetValidation || !snippetProjects || !deserializeLine || !serializeLine) throw new Error('C2 generated config routing call sites are incomplete');

  async function record(id, variants, bindings, operation) {
    const beganAt = Number(process.hrtime.bigint()) / 1e6;
    const nativeBefore = route.nativeEffectCount();
    let timer;
    try {
      const outcome = await Promise.race([
        Promise.resolve().then(operation),
        new Promise((_, reject) => { timer = setTimeout(() => reject(new Error(`1-second watchdog expired: ${id}`)), 1000); })
      ]);
      results.push({ id, variants, status: 'passed', startedMs: beganAt, elapsedMs: Number(process.hrtime.bigint()) / 1e6 - beganAt, nativeEffects: route.nativeEffectCount() - nativeBefore, bindings, outcome: outcome || null });
    } catch (error) {
      results.push({ id, variants, status: 'failed', startedMs: beganAt, elapsedMs: Number(process.hrtime.bigint()) / 1e6 - beganAt, nativeEffects: route.nativeEffectCount() - nativeBefore, bindings, error: String(error.stack || error) });
    } finally {
      clearTimeout(timer);
    }
  }

  await record('C2-01', cases[0][1], [sourceRefs.shim], () => {
    let programRequires = 0;
    let parseCalls = 0;
    const sandbox = {
      require(specifier) {
        if (specifier === '../../../private/c2-bootstrap.cjs') return context.bootstrap;
        if (specifier === 'playwright/lib/program') { programRequires += 1; return { program: { parse() { parseCalls += 1; } } }; }
        throw new Error(`Unexpected selected-entry require: ${specifier}`);
      },
      process: { argv: ['node', 'cli.js'] }
    };
    assert.throws(() => vm.runInNewContext(context.generated[shimName], sandbox, { filename: shimName }), /C2 private execution remains closed/);
    assert.equal(programRequires, 0);
    assert.equal(parseCalls, 0);
    return { programRequires, parseCalls, nativeEffects: 0 };
  });

  await record('C2-02', cases[1][1], [sourceRefs.shim, sourceRefs.config, sourceRefs.core], () => {
    const { closure } = context.manifest;
    assert.ok(closure.copiedEntries >= 180);
    assert.ok(closure.staticEdges.length > 500);
    assert.ok(closure.staticEdges.every(edge => ['node-builtin', 'private-static-resolution', 'optional-watch-excluded-with-refusal', 'optional-missing-native-addon-fallback', 'unsupported-lazy-electron-feature-refused', 'unsupported-proxy-auth-refused'].includes(edge.classification)));
    assert.ok(closure.staticEdges.filter(edge => edge.specifier === 'electron' || edge.specifier === 'electron/index.js').every(edge => edge.classification === 'unsupported-lazy-electron-feature-refused'));
    assert.ok(closure.staticEdges.filter(edge => edge.specifier === 'bufferutil' || edge.specifier === 'utf-8-validate').every(edge => edge.classification === 'optional-missing-native-addon-fallback'));
    assert.ok(closure.staticEdges.filter(edge => edge.specifier === 'kerberos').every(edge => edge.classification === 'unsupported-proxy-auth-refused'));
    assert.ok(closure.dynamicSites.length > 0);
    if (context.dynamicSites?.length) {
      assert.equal(context.dynamicSites.length, 37);
      assert.ok(context.dynamicSites.every(site => ['bound', 'refused'].includes(site.disposition)));
      const refused = context.dynamicSites.find(site => site.siteId === 'DS03');
      const generatedPath = path.join(context.runtimeRoot, refused.generatedFile);
      const generatedLines = fs.readFileSync(generatedPath, 'utf8').split('\n');
      assert.ok(generatedLines[refused.guardLine - 1].includes('DS03'));
      assert.match(generatedLines[refused.guardLine - 1], /dynamicModule/);
      const specifierEffects = { count: 0 };
      let refusal;
      try {
        vm.runInNewContext('privateC2.dynamicModule("DS03", "import", () => { specifierEffects.count += 1; return "/stock/node_modules/unapproved.js"; }, __filename)', { privateC2: context.bootstrap, __filename: generatedPath, specifierEffects });
      } catch (error) {
        refusal = error;
      }
      assert.equal(refusal?.name, 'C2DynamicModuleRefusalError');
      assert.equal(specifierEffects.count, 0);
      assert.equal(route.nativeEffectCount(), 0);
    } else {
      assert.ok(closure.dynamicSites.every(site => site.classification === 'refused-before-program-effects-until-explicitly-bound'));
    }
    assert.ok(!closure.staticEdges.some(edge => edge.specifier.includes('chromium-bidi')));
    assert.match(core, /Private C2 does not support BiDi over CDP without a bound chromium-bidi closure/);
    assert.ok(!closure.staticEdges.some(edge => edge.target && typeof edge.target === 'string' && edge.target.includes('/frontend/node_modules/') && !edge.target.includes(`${context.runtimeRoot}/project/frontend/node_modules/`)));
    const packageRoot = path.join(context.runtimeRoot, 'project/frontend/node_modules');
    for (const pathName of ['@playwright/test/cli.js', 'playwright/lib/program.js', 'playwright/lib/runner/index.js', 'playwright/lib/common/index.js', 'playwright/lib/worker/workerProcessEntry.js', 'playwright-core/lib/coreBundle.js'])
      assert.ok(fs.existsSync(path.join(packageRoot, pathName)), `missing private entry ${pathName}`);
    const packagePath = path.join(packageRoot, '@playwright/test/package.json');
    const packageExports = JSON.parse(fs.readFileSync(packagePath, 'utf8')).exports['.'];
    const requireEntry = createRequire(path.join(context.runtimeRoot, 'project/frontend/package.json')).resolve('@playwright/test');
    const importEntry = path.resolve(path.dirname(packagePath), packageExports.import);
    assert.equal(requireEntry, path.join(packageRoot, '@playwright/test/index.js'));
    assert.equal(importEntry, path.join(packageRoot, '@playwright/test/index.mjs'));
    assert.ok(fs.existsSync(importEntry));
    return { staticEdges: closure.staticEdges.length, dynamicSites: closure.dynamicSites.length, copiedEntries: closure.copiedEntries, cjsEntry: requireEntry, esmEntry: importEntry };
  });

  await record('C2-03', cases[2][1], [sourceRefs.runner, sourceRefs.worker], async () => {
    const supportPath = path.join(context.runtimeRoot, 'project/frontend/private/privateCompletion.cjs');
    const bootstrapPath = path.join(context.runtimeRoot, 'project/frontend/private/c2-bootstrap.cjs');
    const runnerPath = path.join(context.runtimeRoot, 'project/frontend/node_modules/playwright/lib/runner/index.js');
    const workerPath = path.join(context.runtimeRoot, 'project/frontend/node_modules/playwright/lib/worker/workerProcessEntry.js');
    const resolvedFromBootstrap = createRequire(bootstrapPath).resolve('./privateCompletion.cjs');
    const resolvedFromRunner = createRequire(runnerPath).resolve('../../../../private/privateCompletion.cjs');
    const resolvedFromWorker = createRequire(workerPath).resolve('../../../../private/c2-bootstrap.cjs');
    assert.equal(resolvedFromBootstrap, supportPath);
    assert.equal(resolvedFromRunner, supportPath);
    assert.equal(resolvedFromWorker, bootstrapPath);
    assert.strictEqual(context.bootstrap.getPrivateCompletion(), context.support);
    assert.throws(() => context.support.configure({}), /already configured/);
    assert.throws(() => context.bootstrap.installSourceRuntime({ nativeEffectsDisabled: true, paths: context.paths, descriptor: context.descriptor }), /configured more than once/);
    const admittedInputs = route.dynamicProjectInputs();
    assert.equal(admittedInputs.get(path.resolve(context.descriptor.configPath)), context.descriptor.configSHA256);
    assert.equal(route.cacheWritesAllowed(), false);
    const coldConfigReads = [];
    const coldRequire = Object.assign(specifier => { coldConfigReads.push(specifier); return { loaded: specifier }; }, { cache: {} });
    const caller = extractGeneratedFunction(common, 'async function requireOrImport(file)', '\nvar transformInstalled =');
    const coldRequireOrImport = vm.runInNewContext(`${caller}; requireOrImport`, {
      privateC2: context.bootstrap,
      __filename: path.join(context.runtimeRoot, 'project/frontend/node_modules/playwright/lib/common/index.js'),
      installTransformIfNeeded() {},
      fileIsModule: () => false,
      require: coldRequire,
      currentFileDepsCollector: () => undefined
    });
    const coldConfig = await coldRequireOrImport(context.descriptor.configPath);
    assert.equal(coldConfig.loaded, context.descriptor.configPath);
    assert.deepEqual(coldConfigReads, [context.descriptor.configPath]);
    assert.equal(route.cacheWritesAllowed(), false);
    const input = plainConfig(context.descriptor, path.dirname(context.descriptor.configPath));
    evaluateActualLine(snippetValidation, { ...input, privateC2: context.bootstrap, process: { env: input.env } });
    const projects = input.userConfig.projects.map(item => ({ id: item.name, project: { name: item.name, outputDir: path.join(input.packageJsonDir, 'test-results') } }));
    evaluateActualLine(snippetProjects, { privateC2: context.bootstrap, configCLIOverrides: input.configCLIOverrides, projects });
    context.configOverrides = input.configCLIOverrides;
    context.validationInput = input;
    context.routedProjects = projects;
    const manifestRead = context.bootstrap.readPinnedJson(context.descriptor.routeManifestPath, context.descriptor.uid);
    const routeState = manifestRead.value;
    assert.equal(routeState.invocationId, context.descriptor.invocationId);
    const isolatedModules = new Map();
    const childPathTools = loadIsolatedCjs(path.join(context.runtimeRoot, 'project/frontend/private/c2-paths.cjs'), isolatedModules);
    const routeManifestEntry = {
      path: path.relative(context.paths.root, context.descriptor.routeManifestPath),
      kind: 'file',
      dev: manifestRead.identity.dev,
      ino: manifestRead.identity.ino,
      uid: manifestRead.identity.uid,
      mode: manifestRead.identity.mode,
      nlink: manifestRead.identity.nlink,
      bytes: manifestRead.identity.size,
      hash: manifestRead.sha256
    };
    const childPaths = childPathTools.attachInvocation(context.paths.stage, context.paths.root, context.descriptor.invocationId, routeState.entries, [routeManifestEntry]);
    const childBootstrap = loadIsolatedCjs(bootstrapPath, isolatedModules);
    const childSupport = childBootstrap.getPrivateCompletion();
    const childRouteModule = loadIsolatedCjs(path.join(context.runtimeRoot, 'project/frontend/private/c2-route.cjs'), isolatedModules);
    const processToken = crypto.randomUUID();
    const isolatedRoute = childRouteModule.createPrivateRoute(childPaths, context.descriptor, { processToken });
    isolatedRoute.validateSerializedRoute(context.configOverrides);
    const childOutcomePath = path.join(context.paths.root, 'control', `outcomes-${processToken}.ndjson`);
    const childOutcomeActor = `child-outcome-${processToken}`;
    context.paths.registerActor(childOutcomeActor, path.join(context.paths.root, 'control'));
    context.paths.admitProducedFile(childOutcomePath, childOutcomeActor);
    const childRuntime = childBootstrap.installSourceRuntime({ nativeEffectsDisabled: true, paths: childPaths, descriptor: context.descriptor, route: isolatedRoute });
    assert.strictEqual(childRuntime.completion, childSupport);
    assert.notStrictEqual(childSupport, context.support);
    assert.equal(isolatedRoute.projectDirectory('chromium-mobile'), route.projectDirectory('chromium-mobile'));
    assert.throws(() => childBootstrap.beforeWorkerLoad(), /C2 private execution remains closed/);
    assert.ok(worker.indexOf('privateC2.beforeWorkerLoad();') < worker.indexOf('require("../common")'));
    return { supportPath, bootstrapPath, sameExport: true, duplicateConfigurationRefused: true, childModuleIsolated: true, childInvocationId: childPaths.id, childProjectMapBound: true, childOutcomePath, nativeReady: false };
  });

  await record('C2-04', cases[3][1], [sourceRefs.config, sourceRefs.projects], () => {
    const input = context.validationInput;
    const projects = context.routedProjects;
    const originalSemantics = { workers: input.userConfig.workers, retries: input.userConfig.retries, trace: input.userConfig.use.trace, fullyParallel: input.userConfig.fullyParallel };
    const effectiveOutput = context.routeHelpers.effectiveOutput;
    assert.equal(effectiveOutput({ configDir: '/config', outputDir: 'root' }, { outputDir: 'project' }, { outputDir: 'cli' }, '/package'), 'cli');
    assert.equal(effectiveOutput({ configDir: '/config', outputDir: 'root' }, { outputDir: 'project' }, {}, '/package'), path.resolve('/config', 'project'));
    assert.equal(effectiveOutput({ configDir: '/config', outputDir: 'root' }, {}, {}, '/package'), path.resolve('/config', 'root'));
    assert.equal(effectiveOutput({}, {}, {}, '/package'), path.join('/package', 'test-results'));
    let reporterConstruction = 0;
    assert.equal(context.bootstrap.getRuntime().route.cacheWritesAllowed(), true);
    for (const project of projects) {
      const expected = route.projectDirectory(project.project.name);
      assert.equal(project.project.outputDir, expected);
      assert.ok(fs.lstatSync(expected).isDirectory());
    }
    const sourceValidation = common.indexOf('privateC2.getRuntime().route.validateConfig(');
    const sourcePlugins = common.indexOf('this.plugins = (privateConfiguration?.plugins || []).map');
    const sourceReporter = common.indexOf('reporter: takeFirst(');
    assert.ok(sourceValidation >= 0 && sourceValidation < sourcePlugins && sourceValidation < sourceReporter);
    assert.equal(reporterConstruction, 0);
    assert.deepEqual({ workers: input.userConfig.workers, retries: input.userConfig.retries, trace: input.userConfig.use.trace, fullyParallel: input.userConfig.fullyParallel }, originalSemantics);
    return { projectIds: projects.map(project => project.id), projectOutputs: projects.map(project => project.project.outputDir), reporterConstruction, optionsPreserved: originalSemantics };
  });

  await record('C2-05', cases[4][1], [sourceRefs.config], async () => {
    const variants = [
      ['unselected-project-output-refusal', input => { input.userConfig.projects[1].outputDir = '/private/tmp/unapproved-webkit'; }],
      ['cache-override-refusal', input => { input.env.PWTEST_CACHE_DIR = '/tmp/not-private'; }],
      ['profile-override-refusal', input => { input.userConfig.projects[0].use.userDataDir = '/tmp/profile'; }],
      ['custom-reporter-refusal', input => { input.userConfig.reporter = [['./custom-reporter.cjs']]; }],
      ['custom-hook-refusal', input => { input.userConfig.globalSetup = './custom-setup.cjs'; }],
      ['electron-browser-refusal', input => { input.userConfig.projects[0].use.defaultBrowserType = 'electron'; }],
      ['UI-refusal', input => { input.argv = ['--ui']; }],
      ['watch-refusal', input => { input.argv = ['--watch']; }],
      ['proxy-refusal', input => { input.userConfig.projects[0].use.proxy = { server: 'http://127.0.0.1:8080' }; }]
    ];
    const results = [];
    for (const [label, modify] of variants) {
      const item = makeCaseConfig(context, `C2-05-${results.length}`);
      modify(item.input);
      if (label === 'UI-refusal' || label === 'watch-refusal')
        assert.throws(() => context.bootstrap.beforeProgramLoad(item.input.argv), /UI and watch modes are not supported/);
      assert.throws(() => item.route.validateConfig(item.input), /Unsupported|not supported|differs|custom/);
      assert.equal(item.route.cacheWritesAllowed(), false);
      results.push({ label, routeSealed: item.child.sealed, created: item.child.snapshot().entries.length });
    }
    return { variants: results };
  });

  await record('C2-06', cases[5][1], [sourceRefs.config, sourceRefs.projects], () => {
    const originalRoute = context.configOverrides.__herdrC2Route;
    assert.ok(originalRoute);
    const cloned = structuredClone(originalRoute);
    route.validateSerializedRoute({ __herdrC2Route: cloned });
    evaluateActualLine(serializeLine, { privateC2: context.bootstrap, config: { configCLIOverrides: context.configOverrides } });
    evaluateActualLine(deserializeLine, { privateC2: context.bootstrap, data: { configCLIOverrides: context.configOverrides } });
    assert.throws(() => route.validateSerializedRoute({}), /omitted/);
    const tampered = structuredClone(originalRoute);
    tampered.routeId = '0'.repeat(64);
    assert.throws(() => route.validateSerializedRoute({ __herdrC2Route: tampered }), /tampered/);
    const mixed = structuredClone(originalRoute);
    mixed.invocationId = crypto.randomUUID();
    assert.throws(() => route.validateSerializedRoute({ __herdrC2Route: mixed }), /tampered|identity/);
    return { routeId: originalRoute.routeId, projectIds: originalRoute.projectIds, roundTrip: true, invalidRoutesRefused: 3 };
  });

  await record('C2-07', cases[6][1], [sourceRefs.config], () => {
    const runtimeFile = path.join(context.runtimeRoot, 'c2-positive-runtime.txt');
    route.writeRetainedFile(runtimeFile, 'runtime-copy', 'positive-runtime');
    const projectFile = path.join(route.projectDirectory('chromium-mobile'), 'positive-project.txt');
    route.writeRetainedFile(projectFile, 'project-output', 'positive-project');
    const logRoot = route.temporaryPath('logs', 'c2-positive-log');
    const logFile = path.join(logRoot, 'events.ndjson');
    route.writeRetainedFile(logFile, '{"event":"positive"}\n', 'positive-log');
    const reportRoot = route.temporaryPath('reports', 'c2-positive-report');
    const reportFile = path.join(reportRoot, 'report.json');
    route.writeRetainedFile(reportFile, '{"status":"retained"}\n', 'positive-report');
    route.appendOutcome({ type: 'c2-positive-control', status: 'created' });
    const lease = context.paths.objects.get(runtimeFile);
    context.paths.continueFile(runtimeFile, lease.actor, '-continued');
    context.paths.rewriteFile(runtimeFile, lease.actor, 'runtime-copy-final');
    for (const file of [runtimeFile, projectFile, logFile, reportFile]) route.retainAdmittedPath(file);
    assert.equal(fs.readFileSync(runtimeFile, 'utf8'), 'runtime-copy-final');
    assert.ok(fs.existsSync(route.lastRunFile('chromium-mobile')) === false);
    const retentionActor = 'retention-failure-regression';
    context.paths.registerActor(retentionActor, context.paths.root);
    context.paths.acquireSharedDirectory('retention-failure-regression', retentionActor);
    const retentionResult = { phase: 'worker', attempt: 'E1', status: 'passed', invocationId: context.descriptor.invocationId, cases: 24, passed: 24, failed: 0, variants: 0, nativeEffects: 0, verification: {} };
    const completionPath = context.paths.acquireFile('completion-result.json', retentionActor, JSON.stringify(retentionResult)).path;
    const summaryPath = context.paths.acquireFile('attempt-summary.json', retentionActor, JSON.stringify(context.pathTools.attemptSummary(retentionResult))).path;
    const retention = { invocation: { allocatedBytes: 64 * 1024 * 1024 + 512 }, stage: { allocatedBytes: 128 * 1024 * 1024 + 512 }, limits: { invocationBytes: 64 * 1024 * 1024, stageBytes: 128 * 1024 * 1024, campaignBytes: 512 * 1024 * 1024 } };
    assert.equal(context.pathTools.reconcileRetentionFailure(retentionResult, true, retention, 'injected retention failure'), true);
    context.paths.rewriteFile(completionPath, retentionActor, JSON.stringify(retentionResult));
    context.paths.rewriteFile(summaryPath, retentionActor, JSON.stringify(context.pathTools.attemptSummary(retentionResult)));
    const terminal = context.pathTools.terminalRecord(retentionResult, '2026-10-06T00:00:00.000Z');
    assert.equal(JSON.parse(fs.readFileSync(completionPath, 'utf8')).status, 'failed');
    assert.equal(JSON.parse(fs.readFileSync(summaryPath, 'utf8')).status, 'failed');
    assert.equal(terminal.status, 'failed');
    return { retained: [runtimeFile, projectFile, logFile, reportFile], outcomeCount: 1, injectedRetentionFailure: { completionStatus: retentionResult.status, summaryStatus: context.pathTools.attemptSummary(retentionResult).status, terminalStatus: terminal.status, receiptsRewritten: true } };
  });

  await record('C2-08', cases[7][1], [sourceRefs.config], async () => {
    const outcomes = [];
    const variant = async (label, callback) => {
      const scope = createCaseScope(context, `C2-08-${label}`);
      try { await callback(scope); outcomes.push({ label, refused: scope.sealed, entries: scope.snapshot().entries.length }); }
      catch (error) { outcomes.push({ label, refused: scope.sealed, error: String(error.message || error), entries: scope.snapshot().entries.length }); }
      assert.ok(scope.sealed, `${label} must seal its private admission scope`);
    };
    await variant('EEXIST', scope => {
      scope.registerActor('file-writer', scope.root);
      scope.acquireFile('collision', 'file-writer', 'first');
      assert.throws(() => scope.acquireFile('collision', 'file-writer', 'second'), /collision/);
    });
    await variant('symlink', scope => {
      scope.registerActor('dir-writer', scope.root);
      const destination = scope.acquireSharedDirectory('destination', 'dir-writer').path;
      const link = path.join(destination, 'link');
      context.knownNegativePaths.push(link);
      fs.symlinkSync(scope.root, link);
      assert.throws(() => scope.acquireFile('link/payload', 'dir-writer', ''), /exists|identity|directory/);
    });
    await variant('nonregular', scope => {
      scope.registerActor('root-writer', scope.root);
      scope.registerActor('dir-writer', scope.root);
      scope.acquireSharedDirectory('folder', 'dir-writer');
      assert.throws(() => scope.acquireFile('folder', 'root-writer', ''), /collision|directory/);
    });
    await variant('hardlink', scope => {
      scope.registerActor('file-writer', scope.root);
      const file = scope.acquireFile('original', 'file-writer', 'retained').path;
      const hardlink = path.join(scope.root, 'hardlink');
      context.knownNegativePaths.push(file, hardlink);
      fs.linkSync(file, hardlink);
      assert.throws(() => scope.retainPath(file, 'file-writer'), /identity/);
    });
    await variant('uid', scope => {
      scope.registerActor('dir-writer', scope.root);
      const dir = scope.acquireSharedDirectory('readonly', 'dir-writer').path;
      assert.throws(() => context.pathTools.checkedStat(dir, scope.uid + 1, 'directory'), /identity/);
      const originalMkdir = fs.mkdirSync;
      fs.mkdirSync = () => { const error = new Error('injected readonly EACCES at directory admission'); error.code = 'EACCES'; throw error; };
      try { assert.throws(() => scope.acquireDirectory('denied', 'dir-writer'), /EACCES/); }
      finally { fs.mkdirSync = originalMkdir; }
    });
    return { variants: outcomes };
  });

  await record('C2-09', cases[8][1], [sourceRefs.config], async () => {
    const outcomes = [];
    const replace = async (label, action) => {
      const scope = createCaseScope(context, `C2-09-${label}`);
      await action(scope);
      assert.ok(scope.sealed);
      outcomes.push({ label, failure: scope.failure?.message, retained: scope.snapshot().entries.length });
    };
    await replace('parent-replacement', scope => {
      scope.registerActor('tree-writer', scope.root);
      const parent = scope.acquireSharedDirectory('tree', 'tree-writer').path;
      const moved = `${parent}.retained`;
      context.knownNegativePaths.push(parent, moved);
      fs.renameSync(parent, moved);
      fs.symlinkSync(moved, parent);
      assert.throws(() => scope.retainPath(parent, 'tree-writer'));
    });
    await replace('object-replacement', scope => {
      scope.registerActor('file-writer', scope.root);
      const file = scope.acquireFile('object', 'file-writer', 'original').path;
      context.knownNegativePaths.push(file, `${file}.retained`);
      fs.renameSync(file, `${file}.retained`);
      fs.writeFileSync(file, 'replacement', { flag: 'wx', mode: 0o600 });
      assert.throws(() => scope.retainPath(file, 'file-writer'));
    });
    await replace('actor-rebind', scope => {
      scope.registerActor('actor', scope.root);
      scope.acquireSharedDirectory('first', 'actor');
      scope.registerActor('second', scope.root);
      scope.acquireSharedDirectory('second', 'second');
      assert.throws(() => scope.registerActor('actor', path.join(scope.root, 'second')));
    });
    await replace('inspection-error', scope => {
      scope.registerActor('writer', scope.root);
      const originalLstat = fs.lstatSync;
      fs.lstatSync = () => { const error = new Error('injected EIO at pinned identity inspection'); error.code = 'EIO'; throw error; };
      try { assert.throws(() => scope.acquireFile('inspection', 'writer', ''), /EIO/); }
      finally { fs.lstatSync = originalLstat; }
    });
    await replace('ENOSPC', scope => {
      scope.registerActor('writer', scope.root);
      const originalOpen = fs.openSync;
      fs.openSync = () => { const error = new Error('injected ENOSPC at exclusive file create'); error.code = 'ENOSPC'; throw error; };
      try { assert.throws(() => scope.acquireFile('no-space', 'writer', ''), /ENOSPC/); }
      finally { fs.openSync = originalOpen; }
    });
    return { variants: outcomes };
  });

  await record('C2-10', cases[9][1], [sourceRefs.runner, sourceRefs.worker], () => {
    const projectId = 'chromium-mobile';
    const workerIndex = 73;
    const artifactsDir = path.join(route.projectDirectory(projectId), `.playwright-artifacts-${workerIndex}`);
    const params = { projectId, workerIndex, artifactsDir };
    const runnerStart = runner.indexOf('async start() {', runner.indexOf('var WorkerHost = class'));
    const admissionCall = runner.indexOf('admitWorker(this._params)', runnerStart);
    const processCall = runner.indexOf('this.startRunner(this._params', admissionCall);
    assert.ok(runnerStart >= 0 && admissionCall > runnerStart && processCall > admissionCall);
    const admitted = runtime.completion.adapters().admitWorker(params);
    assert.equal(admitted, artifactsDir);
    route.verifyWorkerAdmission(params);
    runtime.completion.adapters().retainWorker(params);
    const refusal = { projectId: 'unbound', workerIndex: 74, artifactsDir: path.join(context.paths.root, 'unbound') };
    assert.throws(() => runtime.completion.adapters().admitWorker(refusal), /not validated|invalid/);
    assert.equal(route.nativeEffectCount(), 0);
    return { projectId, workerIndex, admitted, forkSourceAfterAdmission: true, nativeEffects: 0 };
  });

  await record('C2-11', cases[10][1], [sourceRefs.worker], () => {
    const output = route.projectDirectory('webkit-mobile');
    const createTestInfo = (retry, repeatEachIndex, label) => ({
      project: { name: 'webkit-mobile' },
      outputDir: path.join(output, `${label}-retry-${retry}-repeat-${repeatEachIndex}`),
      testId: `test-${label}-${retry}-${repeatEachIndex}`,
      retry,
      repeatEachIndex
    });
    const first = createTestInfo(0, 0, 'normal');
    route.admitTestOutput(first);
    const attachment = path.join(first.outputDir, 'attachment.txt');
    route.admitTestOutput(first, attachment);
    route.writeRetainedFile(attachment, 'attachment', 'test-attachment');
    const retry = createTestInfo(1, 0, 'retry');
    route.admitTestOutput(retry);
    const repeat = createTestInfo(0, 1, 'repeat');
    route.admitTestOutput(repeat);
    route.retainTestOutput(first);
    route.retainTestOutput(retry);
    route.retainTestOutput(repeat);
    assert.match(worker, /route\.admitTestOutput\(this, outputPath\)/);
    assert.match(worker, /route\.admitTestOutput\(testInfo\)/);
    assert.match(worker, /route\.retainTestOutput\(testInfo\)/);
    assert.doesNotMatch(worker, /removeFolders\(\[testInfo\.outputDir\]\)/);
    return { outputRoots: [first.outputDir, retry.outputDir, repeat.outputDir], attachment, retained: true };
  });

  await record('C2-12', cases[11][1], [sourceRefs.config], () => {
    const cacheRoot = route.transformCacheDir();
    const cacheDir = path.join(cacheRoot, 'c2', '01');
    const actor = 'transform:c2-test:abcdef0';
    const jsPath = path.join(cacheDir, 'source.js');
    const mapPath = path.join(cacheDir, 'source.map');
    const dataPath = path.join(cacheDir, 'source.data');
    const writeFunction = common.match(/function writeCodeCache\(codePath, code, actor\) \{[\s\S]*?\n\}/)?.[0];
    const clearFunction = common.match(/function clearOldCacheEntries\(cacheFolderName, filePathHash\) \{[\s\S]*?\n\}/)?.[0];
    assert.ok(writeFunction && clearFunction);
    const evaluateWriter = vm.runInNewContext(`${writeFunction}; writeCodeCache`, { privateC2: context.bootstrap, calculateSha1: value => sha1(value) });
    evaluateWriter(jsPath, 'compiled-source', actor);
    const readFunction = common.match(/function readCodeCache\(codePath\) \{[\s\S]*?\n\}/)?.[0];
    assert.ok(readFunction);
    const evaluateReader = vm.runInNewContext(`${readFunction}; readCodeCache`, { import_fs: { default: fs }, calculateSha1: value => sha1(value) });
    assert.equal(evaluateReader(jsPath), 'compiled-source');
    const writerLines = common.split('\n').filter(line => line.includes('route.writeCacheFile(sourceMapPath') || line.includes('route.writeCacheFile(dataPath'));
    assert.equal(writerLines.length, 2);
    const [mapWriter, dataWriter] = writerLines;
    const variables = { privateC2: context.bootstrap, sourceMapPath: mapPath, dataPath, actor, map: { version: 3, sources: ['source.ts'] }, data: new Map([['key', 'value']]), JSON, Object };
    evaluateActualLine(mapWriter, variables);
    evaluateActualLine(dataWriter, variables);
    const clear = vm.runInNewContext(`${clearFunction}; clearOldCacheEntries`, { privateC2: context.bootstrap, import_path: { default: path }, cacheDir: cacheRoot });
    clear('c2/01', 'c2-test');
    const js = fs.readFileSync(jsPath, 'utf8');
    assert.equal(js, `// ${sha1('compiled-source')}\ncompiled-source`);
    assert.equal(JSON.parse(fs.readFileSync(mapPath, 'utf8')).version, 3);
    assert.equal(JSON.parse(fs.readFileSync(dataPath, 'utf8')).key, 'value');
    for (const target of [jsPath, mapPath, dataPath]) route.retainAdmittedPath(target);
    assert.doesNotMatch(clearFunction, /rmSync|unlink|rmdir/);
    return { files: [jsPath, mapPath, dataPath], hashes: [js, fs.readFileSync(mapPath, 'utf8'), fs.readFileSync(dataPath, 'utf8')].map(hash), retained: true };
  });

  await record('C2-13', cases[12][1], [sourceRefs.runner], async () => {
    let removeCalls = 0;
    const sourceTask = runUntilClosingBrace(runner, runner.indexOf('function createRemoveOutputDirsTask()'), 'function createRemoveOutputDirsTask()');
    const factory = vm.runInNewContext(`${sourceTask}; createRemoveOutputDirsTask`, { privateC2: context.bootstrap, removeFolders4: async () => { removeCalls += 1; } });
    const task = factory();
    await task.setup({ options: { preserveOutputDir: false }, filteredProjects: [{ id: 'chromium-mobile', project: { name: 'chromium-mobile' } }, { id: 'webkit-mobile', project: { name: 'webkit-mobile' } }] });
    assert.equal(removeCalls, 0);
    await assert.rejects(() => task.setup({ options: { preserveOutputDir: false }, filteredProjects: [{ id: 'invalid', project: { name: 'unbound' } }] }), /not validated|invalid/);
    assert.equal(removeCalls, 0);
    return { clearOutputSource: true, ebusyChildRemoval: false, removeCalls };
  });

  await record('C2-14', cases[13][1], [sourceRefs.core], () => {
    const workerIndex = 74;
    const artifactsDir = path.join(route.projectDirectory('chromium-mobile'), `.playwright-artifacts-${workerIndex}`);
    const params = { projectId: 'chromium-mobile', workerIndex, artifactsDir };
    runtime.completion.adapters().admitWorker(params);
    const options = { artifactsDir };
    const preparedArtifacts = route.browserArtifacts(options.artifactsDir);
    const automaticProfile = route.browserProfile(undefined, false);
    assert.ok(preparedArtifacts.startsWith(artifactsDir));
    assert.ok(automaticProfile.startsWith(path.join(context.paths.root, 'profiles')));
    assert.ok(fs.lstatSync(automaticProfile).isDirectory());
    const testInfo = { project: { name: 'chromium-mobile' }, outputDir: path.join(route.projectDirectory('chromium-mobile'), 'profile-test'), testId: 'profile-test', retry: 0, repeatEachIndex: 0 };
    route.admitTestOutput(testInfo);
    const explicit = path.join(testInfo.outputDir, 'profiles', 'revalidation');
    route.admitTestOutput(testInfo, explicit);
    const explicitProfile = route.browserProfile(explicit, true);
    assert.equal(explicitProfile, explicit);
    assert.throws(() => route.browserProfile(path.join(context.paths.root, 'outside-profile'), true), /outside|admitted/);
    route.retainWorker(params);
    assert.match(core, /route\.browserArtifacts\(options\.artifactsDir\)/);
    assert.match(core, /route\.browserProfile\(userDataDir, isPersistent\)/);
    return { artifacts: preparedArtifacts, automaticProfile, explicitProfile, launchOrdinal: route.snapshot().artifactCount, nativeEffects: 0 };
  });

  await record('C2-15', cases[14][1], [sourceRefs.core, sourceRefs.worker], () => {
    const traceRoot = route.traceDirectory('c2rawtrace');
    const rawTrace = path.join(traceRoot, 'trace.raw');
    const actor = route.admitArtifactIntent(rawTrace);
    fs.writeFileSync(rawTrace, 'raw-trace', { flag: 'wx', mode: 0o600 });
    route.completeArtifactIntent(rawTrace);
    route.retainAdmittedPath(rawTrace);
    const stack = route.traceDirectory('c2stacktrace');
    assert.ok(fs.lstatSync(stack).isDirectory());
    const tempTrace = path.join(context.paths.root, 'profiles', 'discarded-trace.zip');
    route.admitWritableFile(tempTrace, 'trace-discard-source');
    fs.writeFileSync(tempTrace, 'raw-zip', { flag: 'r+' });
    const stop = worker.slice(worker.indexOf('if (this._shouldAbandonTrace())'), worker.indexOf('const zipFile = new yazl.ZipFile()'));
    assert.match(stop, /route\.retainAdmittedPath\(file\)/);
    assert.doesNotMatch(stop, /attachments\.push|new yazl\.ZipFile/);
    route.retainAdmittedPath(tempTrace);
    assert.ok(fs.existsSync(rawTrace) && fs.existsSync(tempTrace));
    return { rawTrace, stack, tempTrace, artifactActor: actor, retained: true, passingDiscardAttachmentCount: 0 };
  });

  await record('C2-16', cases[15][1], [sourceRefs.worker], () => {
    const outputRoot = path.join(route.projectDirectory('webkit-mobile'), 'trace-merge-test');
    const info = { project: { name: 'webkit-mobile' }, outputDir: outputRoot, testId: 'trace-merge', retry: 0, repeatEachIndex: 0 };
    route.admitTestOutput(info);
    const source = path.join(route.projectDirectory('webkit-mobile'), '.playwright-artifacts-73', 'source-trace.zip');
    route.admitWritableFile(source, 'single-trace-source');
    fs.writeFileSync(source, 'single-source', { flag: 'r+' });
    const moved = path.join(outputRoot, 'single.zip');
    route.relocateAdmittedFile(source, moved);
    assert.equal(fs.readFileSync(moved, 'utf8'), 'single-source');
    const isolated = createCaseScope(context, 'C2-16-collision');
    const isolatedRoute = context.routeFactory(isolated, { ...context.descriptor, invocationId: isolated.id, root: isolated.root, uid: isolated.uid });
    const collisionSource = path.join(isolated.root, 'profiles', 'collision-source.zip');
    isolatedRoute.admitWritableFile(collisionSource, 'collision-source');
    assert.throws(() => isolatedRoute.relocateAdmittedFile(collisionSource, collisionSource), /collision/);
    assert.ok(isolated.sealed);
    const inputA = path.join(route.projectDirectory('webkit-mobile'), '.playwright-artifacts-73', 'source-a.zip');
    const inputB = path.join(route.projectDirectory('webkit-mobile'), '.playwright-artifacts-73', 'source-b.zip');
    route.admitWritableFile(inputA, 'merge-a');
    route.admitWritableFile(inputB, 'merge-b');
    fs.writeFileSync(inputA, 'a', { flag: 'r+' });
    fs.writeFileSync(inputB, 'b', { flag: 'r+' });
    const merged = path.join(outputRoot, 'merged.zip');
    route.admitWritableFile(merged, 'merge-output');
    fs.writeFileSync(merged, 'ab', { flag: 'r+' });
    route.retainAdmittedPath(inputA);
    route.retainAdmittedPath(inputB);
    route.retainAdmittedPath(merged);
    assert.ok(fs.existsSync(inputA) && fs.existsSync(inputB));
    return { relocated: { source, destination: moved }, collisionRefused: true, mergeInputsRetained: [inputA, inputB], merged };
  });

  await record('C2-17', cases[16][1], [sourceRefs.core, sourceRefs.runner], () => {
    const uploadRoot = route.admitUploadDirectory('c2-upload');
    const uploadBase = route.admitUploadRoot(uploadRoot, undefined);
    const upload = route.admitUploadFile(uploadBase, 'nested/input.txt', 'upload-item');
    fs.writeFileSync(upload, 'upload-data', { flag: 'r+' });
    route.retainAdmittedPath(upload);
    const downloadRoot = route.admitDownloadDirectory('c2-download');
    const download = path.join(downloadRoot, 'download-uuid');
    const downloadActor = route.admitArtifactIntent(download);
    fs.writeFileSync(download, 'download-data', { flag: 'wx', mode: 0o600 });
    route.completeArtifactIntent(download);
    route.retainAdmittedPath(download);
    const lastRun = route.lastRunFile('chromium-mobile');
    route.writeRetainedFile(lastRun, JSON.stringify({ failedTests: [] }), 'last-run-c2');
    route.retainAdmittedPath(lastRun);
    assert.match(runner, /route\.lastRunFile\(project\.id \|\| project\.project\.name\)/);
    assert.match(core, /route\.admitArtifactIntent\(localPath\)/);
    return { upload, download, downloadActor, lastRun, allRetained: [upload, download, lastRun] };
  });

  await record('C2-18', cases[17][1], [sourceRefs.fixture], () => {
    assert.match(fixture, /testInfo\.outputPath\('profiles', 'revalidation'\)/);
    assert.doesNotMatch(fixture, /mkdtemp|tmpdir|await rm\(/);
    assert.equal(fixture.split('expect(').length, context.originalReleaseFixture.split('expect(').length);
    assert.match(fixture, /await context\?\.close\(\)/);
    assert.match(fixture, /server\.close\(\(error\) => error \? reject\(error\) : resolve\(\)\)/);
    const profileInfo = { project: { name: 'chromium-mobile' }, outputDir: path.join(route.projectDirectory('chromium-mobile'), 'release-fixture'), testId: 'release-fixture', retry: 0, repeatEachIndex: 0 };
    const profilePath = path.join(profileInfo.outputDir, 'profiles', 'revalidation');
    route.admitTestOutput(profileInfo, profilePath);
    assert.equal(route.browserProfile(profilePath, true), profilePath);
    return { fixtureSHA256: hash(fixture), profilePath, assertionCount: fixture.split('expect(').length - 1, serverAndContextClosePreserved: true };
  });

  await record('C2-19', cases[18][1], [sourceBinding(context, 'frontend/scripts/run-browser-tests.sh', 'HERDR_FULLSCREEN_C2_PRIVATE')], () => {
    const wrapper = context.wrapperSource;
    const branchStart = wrapper.indexOf('if [ "${HERDR_FULLSCREEN_C2_PRIVATE:-}" = 1 ]; then');
    const branchEnd = wrapper.indexOf('\n\nSCRIPT_DIR=', branchStart);
    const stockAfter = wrapper.slice(branchEnd + 2);
    assert.ok(branchStart > wrapper.indexOf('set -euo pipefail'));
    assert.ok(branchEnd > branchStart && branchEnd < wrapper.indexOf('SCRIPT_DIR='));
    const branch = wrapper.slice(branchStart, branchEnd);
    assert.match(branch, /cd "\$PRIVATE_ROOT\/runtime\/project\/frontend"/);
    assert.match(branch, /exec node "\$PRIVATE_CLI" test/);
    assert.doesNotMatch(branch, /bun x playwright/);
    assert.match(branch, /exit 2/);
    const originalWrapper = context.wrapperBaseline;
    assert.equal(hash(wrapper.slice(0, branchStart) + stockAfter), hash(originalWrapper));
    assert.equal(route.nativeEffectCount(), 0);
    return { branchStart, branchEnd, privateCLI: '$PRIVATE_ROOT/runtime/project/frontend/node_modules/@playwright/test/cli.js', stockBytesPreserved: true };
  });

  await record('C2-20', cases[19][1], [sourceRefs.runner, sourceRefs.shim], () => {
    assert.equal(context.manifest.c1.record, '3ea2ca12-526b-46d1-8efa-31dfdbccf3d4');
    assert.equal(context.manifest.c1.runnerSHA256, 'f1ebbae7ab23305089c726578ad54be271ad46dc33b438121f7fd6f141f7c344');
    assert.equal(context.manifest.c1.cases, 79);
    assert.equal(context.manifest.generatedFiles.filter(file => file.source.endsWith('/privateCompletion.cjs')).length, 1);
    assert.equal(context.manifest.c1.variants, 80);
    const scan = context.manifest.closure.dynamicSiteScan;
    assert.equal(scan.original.scannedSites, 37);
    assert.equal(scan.original.matchedSites, 37);
    assert.deepEqual(scan.original.unscannedSiteIds, []);
    const ds01 = scan.original.matches.find(site => site.siteId === 'DS01');
    assert.equal(ds01.inventoryCoordinate.line, 1107);
    assert.equal(ds01.originalCoordinate.line, 1109);
    assert.equal(ds01.inventoryCoordinate.representation, 'frozen-transformed-source');
    assert.equal(ds01.originalCoordinate.representation, 'raw-original-source');
    assert.equal(ds01.generatedCoordinate.representation, 'generated-private-source');
    assert.equal(ds01.generatedExpressionCoordinate.representation, 'generated-private-source-dynamic-expression');
    assert.equal(ds01.generatedExpressionCoordinate.file, ds01.generatedFile);
    const expectedGeneratedOccurrences = context.dynamicSites.filter(site => site.enforcedBy.length === 0).length + context.dynamicSites.reduce((total, site) => total + (site.generatedExpansions?.length || 0), 0);
    assert.equal(scan.generated.scannedSites, expectedGeneratedOccurrences);
    assert.equal(scan.generated.matchedSites, expectedGeneratedOccurrences);
    assert.deepEqual(scan.generated.unscannedSiteIds, []);
    const ds02 = context.dynamicSites.find(site => site.siteId === 'DS02');
    assert.equal(ds02.generatedGuardOccurrences.length, 3);
    assert.equal(ds02.generatedGuardOccurrences.filter(item => item.provenance.type === 'frozen-inventory-occurrence').length, 1);
    assert.deepEqual(ds02.generatedExpansions.map(item => item.expansionId).sort(), ['DS02-ESM-import', 'DS02-ESM-preflight']);
    assert.ok(ds02.generatedExpansions.every(item => item.generatedCoordinate?.line > 0 && item.originalCoordinate?.sourceOffset >= 0));
    assert.throws(() => assertDynamicSiteScanClosed([{ from: 'frontend/node_modules/new-module.js', expression: 'computed', line: 1 }], context.dynamicSites), /absent from the frozen inventory/);
    const coordinatePath = 'frontend/node_modules/coordinate-fixture.js';
    const coordinateSource = 'require(\n  create(\n    nested("value)")\n  )\n);\nrequire(\n  create(\n    nested("value)")\n  )\n);\n';
    const coordinateScanned = scanDynamicSites(coordinatePath, coordinateSource);
    const coordinateExpressionSHA256 = hash(Buffer.from(coordinateScanned[0].expression));
    const coordinateInventory = [
      { siteId: 'DUP01', from: coordinatePath, line: 20, expression: coordinateScanned[0].expression, expressionSHA256: coordinateExpressionSHA256, originalFileSHA256: hash(Buffer.from(coordinateSource)) },
      { siteId: 'DUP02', from: coordinatePath, line: 21, expression: coordinateScanned[1].expression, expressionSHA256: coordinateExpressionSHA256, originalFileSHA256: hash(Buffer.from(coordinateSource)) }
    ];
    const coordinateScan = assertOriginalDynamicSiteScanClosed(coordinateScanned, coordinateInventory, { [coordinatePath]: Buffer.from(coordinateSource) });
    assert.deepEqual(coordinateScan.matches.map(site => [site.siteId, site.inventoryCoordinate.line, site.originalCoordinate.line, site.originalCoordinate.occurrence]), [['DUP01', 20, 1, 0], ['DUP02', 21, 7, 1]]);
    assert.ok(coordinateScan.matches.every(site => site.occurrenceMapping.unambiguous));
    assert.throws(() => assertOriginalDynamicSiteScanClosed(coordinateScanned, coordinateInventory, { [coordinatePath]: Buffer.from(`${coordinateSource}// changed`) }), /original source hash changed/);
    assert.throws(() => assertOriginalDynamicSiteScanClosed([{ ...coordinateScanned[0], from: 'frontend/node_modules/renamed.js' }], coordinateInventory, { [coordinatePath]: Buffer.from(coordinateSource) }), /absent from the frozen inventory/);
    assert.throws(() => assertOriginalDynamicSiteScanClosed(coordinateScanned.map(site => ({ ...site, expression: 'changed' })), coordinateInventory, { [coordinatePath]: Buffer.from(coordinateSource) }), /absent from the frozen inventory/);
    assert.throws(() => assertOriginalDynamicSiteScanClosed(coordinateScanned.slice(0, 1), coordinateInventory, { [coordinatePath]: Buffer.from(coordinateSource) }), /occurrence count differs/);
    assert.throws(() => assertOriginalDynamicSiteScanClosed([...coordinateScanned, coordinateScanned[0]], coordinateInventory, { [coordinatePath]: Buffer.from(coordinateSource) }), /occurrence count differs/);
    const duplicateSemantics = assertOriginalDynamicSiteSemanticsUnambiguous(coordinateScan, [
      { siteId: 'DUP01', disposition: 'bound', approvedTargets: [{ specifier: 'one' }], enforcedBy: [], refusal: null },
      { siteId: 'DUP02', disposition: 'refused', approvedTargets: [], enforcedBy: [], refusal: { errorName: 'C2DynamicModuleRefusalError' } }
    ]);
    assert.equal(duplicateSemantics.duplicateGroups[0].mappingUnambiguous, true);
    assert.equal(duplicateSemantics.duplicateGroups[0].semanticsDiffer, true);
    const makeDuplicateGuards = ids => coordinateScanned.map((site, index) => `require(privateC2.dynamicModule(${JSON.stringify(ids[index])}, "require", () => (${site.callExpression}), __filename));`).join('\n');
    const duplicateSites = coordinateScan.matches.map((match, index) => ({
      siteId: match.siteId,
      from: coordinatePath,
      generatedFile: 'frontend/private/coordinate-fixture.js',
      generatedSHA256: hash(Buffer.from(makeDuplicateGuards(['DUP01', 'DUP02']))),
      guardLine: 1,
      expressionSHA256: coordinateExpressionSHA256,
      originalCoordinate: match.originalCoordinate,
      generatedExpansions: [],
      enforcedBy: []
    }));
    const duplicateGuardSource = makeDuplicateGuards(['DUP01', 'DUP02']);
    const duplicateGenerated = assertGeneratedDynamicScanClosed(scanDynamicSites(coordinatePath, duplicateGuardSource), duplicateSites, { [coordinatePath]: duplicateGuardSource });
    assert.deepEqual(duplicateGenerated.generatedGuards.map(guard => guard.siteId), ['DUP01', 'DUP02']);
    assert.equal(duplicateGenerated.generatedGuards.length, 2);
    assert.throws(() => assertGeneratedDynamicScanClosed(scanDynamicSites(coordinatePath, makeDuplicateGuards(['DUP02', 'DUP01'])), duplicateSites.map(site => ({ ...site, generatedSHA256: hash(Buffer.from(makeDuplicateGuards(['DUP02', 'DUP01']))) })), { [coordinatePath]: makeDuplicateGuards(['DUP02', 'DUP01']) }), /reordered/);
    const missingGuardSource = coordinateSource;
    assert.throws(() => assertGeneratedDynamicScanClosed(scanDynamicSites(coordinatePath, missingGuardSource), duplicateSites, { [coordinatePath]: missingGuardSource }), /absent from the frozen inventory/);
    const extraGuardSource = `${makeDuplicateGuards(['DUP01', 'DUP02'])}\nrequire(privateC2.dynamicModule("EXTRA", "require", () => (extra), __filename));`;
    assert.throws(() => assertGeneratedDynamicScanClosed(scanDynamicSites(coordinatePath, extraGuardSource), duplicateSites.map(site => ({ ...site, generatedSHA256: hash(Buffer.from(extraGuardSource)) })), { [coordinatePath]: extraGuardSource }), /wrong or unbound inner guard ID/);
    const wrongGuardExpression = 'require(privateC2.dynamicModule("DUP01", "require", () => (different), __filename));';
    assert.throws(() => assertGeneratedDynamicScanClosed(scanDynamicSites(coordinatePath, wrongGuardExpression), [{ ...duplicateSites[0], generatedSHA256: hash(Buffer.from(wrongGuardExpression)) }], { [coordinatePath]: wrongGuardExpression }), /does not match its frozen provenance/);
    const assetSource = 'import(__c2DynamicAsset("DS35", `./asset.js`));';
    const assetSite = { siteId: 'DS35', from: 'frontend/node_modules/asset.js', generatedFile: 'frontend/private/asset.js', generatedSHA256: hash(Buffer.from(assetSource)), guardLine: 1, expressionSHA256: hash(Buffer.from('`./asset.js`')), originalCoordinate: { kind: 'import' }, generatedExpansions: [], enforcedBy: [] };
    const assetScan = assertGeneratedDynamicScanClosed(scanDynamicSites(assetSite.from, assetSource), [assetSite], { [assetSite.from]: assetSource });
    assert.equal(assetScan.generatedGuards[0].guardStyle, '__c2DynamicAsset');
    const wrongAssetSource = 'import(__c2DynamicAsset("DS35", `./other.js`));';
    assert.throws(() => assertGeneratedDynamicScanClosed(scanDynamicSites(assetSite.from, wrongAssetSource), [{ ...assetSite, generatedSHA256: hash(Buffer.from(wrongAssetSource)) }], { [assetSite.from]: wrongAssetSource }), /does not match its frozen provenance/);
    const foreignGuardSource = 'require(privateC2.dynamicModule("DS02", "require", () => (computed), __filename));';
    assert.throws(() => assertGeneratedDynamicScanClosed(scanDynamicSites(coordinatePath, foreignGuardSource), [{ ...duplicateSites[0], generatedSHA256: hash(Buffer.from(foreignGuardSource)) }], { [coordinatePath]: foreignGuardSource }), /wrong or unbound inner guard ID/);
    assert.equal(route.nativeEffectCount(), 0);
    assert.throws(() => context.bootstrap.assertExecutionReady(), /C2 private execution remains closed/);
    assert.equal(new Set(context.priorInvocationIds.concat(context.descriptor.invocationId)).size, context.priorInvocationIds.length + 1);
    return { reusedC1: context.manifest.c1, nativeReady: false, nativeEffects: 0, invocationId: context.descriptor.invocationId };
  });

  const caseResults = results.map((result, index) => ({ ...result, variants: cases[index][1] }));
  const expectedIds = cases.map(([id]) => id);
  if (caseResults.length !== 20 || caseResults.some((result, index) => result.id !== expectedIds[index] || result.nativeEffects !== 0))
    throw new Error('C2 case IDs, count, or native-effect counter differ from the bounded plan');
  const variants = cases.reduce((total, [, labels]) => total + labels.length, 0);
  if (variants !== 83) throw new Error('C2 required variant count differs from the supplemental bounded acceptance');
  return {
    schema: 1,
    phase: 'C2-native-disabled-source',
    invocationId: context.descriptor.invocationId,
    status: caseResults.every(result => result.status === 'passed') ? 'passed' : 'failed',
    expectedCases: 20,
    passedCases: caseResults.filter(result => result.status === 'passed').length,
    failedCases: caseResults.filter(result => result.status !== 'passed').length,
    variantCount: variants,
    caseIds: expectedIds,
    cases: caseResults,
    sourceBindings: sourceRefs,
    nativeEffects: caseResults.reduce((sum, result) => sum + result.nativeEffects, 0),
    durationMs: caseResults.reduce((sum, result) => sum + result.elapsedMs, 0),
    pathScopes: context.childScopes.map(scope => ({ root: scope.root, invocationId: scope.id, snapshot: scope.pathPolicy.snapshot() }))
  };
}

export async function runC2DynamicTests(context) {
  const sites = context.dynamicSites;
  const byId = new Map(sites.map(site => [site.siteId, site]));
  const runtime = context.bootstrap.getRuntime();
  const route = runtime.route;
  const results = [];

  async function record(id, variants, operation) {
    const startedMs = Number(process.hrtime.bigint()) / 1e6;
    const before = route.nativeEffectCount();
    try {
      const outcome = await operation();
      results.push({ id, variants, status: 'passed', nativeEffects: route.nativeEffectCount() - before, elapsedMs: Number(process.hrtime.bigint()) / 1e6 - startedMs, outcome });
    } catch (error) {
      results.push({ id, variants, status: 'failed', nativeEffects: route.nativeEffectCount() - before, elapsedMs: Number(process.hrtime.bigint()) / 1e6 - startedMs, error: String(error.stack || error) });
    }
  }

  function generatedSite(siteId) {
    const site = byId.get(siteId);
    if (!site) throw new Error(`C2 generated dynamic site is missing: ${siteId}`);
    const filename = path.join(context.runtimeRoot, site.generatedFile);
    const bytes = fs.readFileSync(filename);
    assert.equal(hash(bytes), site.generatedSHA256);
    const source = bytes.toString('utf8');
    const lines = source.split('\n');
    const occurrence = site.generatedGuardOccurrences?.find(item => item.provenance.type === 'frozen-inventory-occurrence');
    const guardOffset = occurrence?.guardStart ?? source.indexOf(`privateC2.enforceDynamicSite(${JSON.stringify(siteId)}`);
    const guardLineNumber = occurrence?.line ?? site.guardLine;
    const guardLine = lines[guardLineNumber - 1];
    assert.ok(guardOffset >= 0 && guardLine?.includes(siteId), `generated guard line missing ${siteId}`);
    return { site, filename, bytes, lines, guardLine, guardLineNumber, guardOffset, occurrence };
  }

  function invokeGuard(siteId, kind, specifier, filename) {
    const result = vm.runInNewContext(`privateC2.dynamicModule(${JSON.stringify(siteId)}, ${JSON.stringify(kind)}, () => specifier, __filename)`, {
      privateC2: context.bootstrap,
      specifier,
      __filename: filename
    });
    return result;
  }

  function invokeGeneratedGuard(generated) {
    const { site, filename, bytes, lines, guardLine } = generated;
    const source = bytes.toString('utf8');
    if (site.enforcedBy.length) {
      const marker = `privateC2.enforceDynamicSite(${JSON.stringify(site.siteId)}`;
      const offset = source.indexOf(marker);
      assert.ok(offset >= 0, `${site.siteId} generated delegation guard is missing`);
      assert.equal(vm.runInNewContext(extractBalancedCall(source, offset), { privateC2: context.bootstrap }), true);
      const delegatedTargets = site.enforcedBy.map(siteId => {
        const delegatedSite = byId.get(siteId);
        const target = delegatedSite?.approvedTargets[0];
        assert.ok(target, `${site.siteId} delegates to an unbound guard: ${siteId}`);
        const resolved = invokeGuard(siteId, 'require', target.specifier, filename);
        assert.equal(resolved, target.builtin ? target.specifier : path.join(context.runtimeRoot, target.resolvedPath));
        return { siteId, target: target.builtin ? target.specifier : target.resolvedPath };
      });
      return { resolvedTarget: delegatedTargets, refusal: null };
    }
    if (['DS35', 'DS36', 'DS37'].includes(site.siteId)) {
      const targetsLine = lines.find(line => line.startsWith('const __c2DynamicAssetTargets = '));
      const helper = extractGeneratedHelper(source, 'function __c2DynamicAsset(');
      const target = site.approvedTargets[0];
      const specifier = site.disposition === 'bound' ? target.assetSpecifier : '/private/tmp/unapproved-c2-asset';
      try {
        const resolved = vm.runInNewContext(`${targetsLine}\n${helper}; __c2DynamicAsset(${JSON.stringify(site.siteId)}, specifier)`, { specifier });
        assert.equal(resolved, target.assetSpecifier);
        return { resolvedTarget: target.resolvedPath, refusal: null };
      } catch (error) {
        return { resolvedTarget: null, refusal: error };
      }
    }
    const offset = generated.guardOffset;
    assert.ok(offset >= 0, `${site.siteId} generated module guard is missing`);
    const call = replaceThirdArgument(extractBalancedCall(source, offset));
    const kind = /dynamicModule\("DS[0-9]+", "(import|require)"/.exec(guardLine)?.[1];
    assert.ok(kind, `${site.siteId} generated loader kind is missing`);
    const target = site.approvedTargets[0];
    const specifier = target?.specifier || '/private/tmp/unapproved-c2-module';
    try {
      const resolved = vm.runInNewContext(call, { privateC2: context.bootstrap, specifier, __filename: filename });
      if (target?.builtin === true) assert.equal(resolved, target.specifier);
      else if (target) assert.equal(kind === 'import' ? fileURLToPath(resolved) : resolved, path.join(context.runtimeRoot, target.resolvedPath));
      return { resolvedTarget: target?.builtin ? target.specifier : target?.resolvedPath || null, refusal: null };
    } catch (error) {
      return { resolvedTarget: null, refusal: error };
    }
  }

  await record('C2-21', sites.map(site => `${site.siteId}-guarded`), () => {
    const outcomes = [];
    for (const site of sites) {
      const generated = generatedSite(site.siteId);
      assert.match(generated.guardLine, /dynamicModule|enforceDynamicSite|__c2DynamicAsset/);
      const invocation = invokeGeneratedGuard(generated);
      if (site.disposition === 'refused') {
        assert.equal(invocation.refusal?.name, 'C2DynamicModuleRefusalError', `${site.siteId} generated guard did not refuse`);
        assert.equal(invocation.refusal.siteId, site.siteId);
      } else {
        assert.equal(invocation.refusal, null, `${site.siteId} generated guard refused its bound target`);
        assert.ok(invocation.resolvedTarget, `${site.siteId} generated guard did not resolve a target`);
      }
      outcomes.push({ siteId: site.siteId, disposition: site.disposition, invoked: true, resolvedTarget: invocation.resolvedTarget, refusal: invocation.refusal?.name || null, effectsBeforeDecision: 0, generatedSHA256: hash(generated.bytes), guardLine: site.guardLine });
    }
    return { sites: outcomes, sameInvocationRuntime: true };
  });

  const allowedSites = sites.filter(site => site.approvedTargets.length > 0);
  await record('C2-22', allowedSites.map(site => `${site.siteId}-allowed`), () => {
    const outcomes = [];
    for (const site of allowedSites) {
      const generated = generatedSite(site.siteId);
      const target = site.approvedTargets[0];
      const split = Math.max(1, Math.floor(target.specifier.length / 2));
      const computed = `${target.specifier.slice(0, split)}${target.specifier.slice(split)}`;
      const resolvedRequire = invokeGuard(site.siteId, 'require', computed, generated.filename);
      const resolvedImport = invokeGuard(site.siteId, 'import', computed, generated.filename);
      if (target.builtin === true) {
        assert.equal(resolvedRequire, target.specifier);
        assert.equal(resolvedImport, target.specifier);
        assert.ok(Module.builtinModules.includes(target.specifier.replace(/^node:/, '')));
        createRequire(generated.filename)(resolvedRequire);
      } else {
        assert.equal(resolvedRequire, path.join(context.runtimeRoot, target.resolvedPath));
        assert.equal(fileURLToPath(resolvedImport), path.join(context.runtimeRoot, target.resolvedPath));
        const targetBytes = fs.readFileSync(resolvedRequire);
        assert.equal(hash(targetBytes), target.sha256);
        assert.ok(resolvedRequire.startsWith(`${context.runtimeRoot}${path.sep}`));
        if (['DS35', 'DS36', 'DS37'].includes(site.siteId)) {
          const targetLine = generated.lines.find(line => line.startsWith('const __c2DynamicAssetTargets = '));
          const assetHelper = extractGeneratedHelper(generated.bytes.toString('utf8'), 'function __c2DynamicAsset(');
          assert.ok(targetLine);
          const assetSpecifier = vm.runInNewContext(`${targetLine}\n${assetHelper}; __c2DynamicAsset(${JSON.stringify(site.siteId)}, ${JSON.stringify(target.assetSpecifier)})`);
          assert.equal(assetSpecifier, target.assetSpecifier);
          assert.equal(path.resolve(path.dirname(generated.filename), assetSpecifier), resolvedRequire);
        }
      }
      outcomes.push({ siteId: site.siteId, disposition: site.disposition, invoked: true, resolvedTarget: target.builtin ? target.specifier : target.resolvedPath, refusal: null, effectsBeforeDecision: 0 });
    }
    return { sites: outcomes, requireAndImportForms: true };
  });

  await record('C2-23', sites.map(site => `${site.siteId}-refused`), () => {
    const generatedSites = sites.map(site => generatedSite(site.siteId));
    const filesystemMethods = ['readFileSync', 'statSync', 'lstatSync', 'realpathSync', 'existsSync', 'openSync', 'accessSync', 'readdirSync', 'readlinkSync', 'writeFileSync', 'appendFileSync', 'writeSync', 'mkdirSync', 'renameSync', 'unlinkSync', 'rmSync', 'truncateSync', 'ftruncateSync', 'symlinkSync', 'linkSync'];
    const originalFilesystem = Object.fromEntries(filesystemMethods.map(name => [name, fs[name]]));
    const originalResolveFilename = Module._resolveFilename;
    const effects = { filesystem: 0, resolve: 0, require: 0, import: 0, native: 0 };
    for (const name of filesystemMethods) fs[name] = function (...args) { effects.filesystem += 1; return originalFilesystem[name].apply(this, args); };
    Module._resolveFilename = function (...args) { effects.resolve += 1; return originalResolveFilename.apply(this, args); };
    const outcomes = [];
    try {
      for (const generated of generatedSites) {
        const site = generated.site;
        const source = generated.bytes.toString('utf8');
        const kind = /dynamicModule\("DS[0-9]+", "(import|require)"/.exec(generated.guardLine)?.[1] || 'require';
        const inputs = [
          path.join(context.runtimeRoot, 'project/frontend/node_modules/stock-leak.js'),
          '/private/tmp/outside-c2/unapproved.js',
          '../outside-c2/unapproved.js',
          'unapproved-private-package'
        ];
        let refusal;
        for (const specifier of inputs) {
          let error;
          if (site.enforcedBy.length) {
            const marker = `privateC2.enforceDynamicSite(${JSON.stringify(site.siteId)}`;
            const offset = source.indexOf(marker);
            assert.ok(offset >= 0, `${site.siteId} generated delegation guard is missing`);
            const guardCall = extractBalancedCall(source, offset);
            vm.runInNewContext(guardCall, { privateC2: context.bootstrap });
            for (const downstream of site.enforcedBy) {
              try { invokeGuard(downstream, kind, specifier, generated.filename); } catch (caught) { error = caught; break; }
            }
          } else if (['DS35', 'DS36', 'DS37'].includes(site.siteId)) {
            const targetsLine = generated.lines.find(line => line.startsWith('const __c2DynamicAssetTargets = '));
            assert.ok(targetsLine, `${site.siteId} private asset target table is missing`);
            const assetHelper = extractGeneratedHelper(source, 'function __c2DynamicAsset(');
            try { vm.runInNewContext(`${targetsLine}\n${assetHelper}; __c2DynamicAsset(${JSON.stringify(site.siteId)}, specifier)`, { specifier }); } catch (caught) { error = caught; }
          } else {
            const offset = generated.guardOffset;
            assert.ok(offset >= 0, `${site.siteId} generated module guard is missing`);
            const generatedCall = replaceThirdArgument(extractBalancedCall(source, offset));
            try { vm.runInNewContext(generatedCall, { privateC2: context.bootstrap, specifier, __filename: generated.filename, require() { effects.require += 1; throw new Error('require ran before the guard decision'); }, dynamicImport() { effects.import += 1; throw new Error('import ran before the guard decision'); } }); } catch (caught) { error = caught; }
          }
          assert.equal(error?.name, 'C2DynamicModuleRefusalError', `${site.siteId} accepted ${specifier}`);
          assert.ok(site.enforcedBy.length ? site.enforcedBy.includes(error.siteId) : error.siteId === site.siteId);
          refusal = error.name;
        }
        outcomes.push({ siteId: site.siteId, disposition: site.disposition, invoked: true, resolvedTarget: null, refusal, effectsBeforeDecision: 0 });
      }
    } finally {
      for (const name of filesystemMethods) fs[name] = originalFilesystem[name];
      Module._resolveFilename = originalResolveFilename;
    }
    assert.deepEqual(effects, { filesystem: 0, resolve: 0, require: 0, import: 0, native: 0 });
    assert.equal(route.nativeEffectCount(), 0);
    return { sites: outcomes, effects };
  });

  const callerVariants = ['require-config', 'import-config-esm', 'transform-bundle-load', 'loader-emitted-require', 'loader-emitted-import', 'package-json-read', 'refused-stock-leak', 'refused-unapproved-computed'];
  await record('C2-24', callerVariants, async () => {
    const outcomes = [];
    const commonName = 'frontend/node_modules/playwright/lib/common/index.js';
    const commonPath = path.join(context.runtimeRoot, 'project/frontend/node_modules/playwright/lib/common/index.js');
    const commonSource = context.generated[commonName];
    const caller = extractGeneratedFunction(commonSource, 'async function requireOrImport(file)', '\nvar transformInstalled =');
    const configPath = context.descriptor.configPath;
    const required = [];
    const fakeRequire = Object.assign(specifier => { required.push(specifier); return { loaded: specifier }; }, { cache: {} });
    const requireConfig = vm.runInNewContext(`${caller}; requireOrImport`, {
      privateC2: context.bootstrap,
      __filename: commonPath,
      installTransformIfNeeded() {},
      fileIsModule: () => false,
      require: fakeRequire,
      currentFileDepsCollector: () => undefined
    });
    const configResult = await requireConfig(configPath);
    assert.equal(configResult.loaded, configPath);
    assert.deepEqual(required, [configPath]);
    outcomes.push({ variant: 'require-config', siteId: 'DS02', disposition: 'bound', invoked: true, resolvedTarget: path.relative(context.runtimeRoot, configPath), refusal: null, effectsBeforeDecision: 0 });

    const imported = [];
    const importRequire = Object.assign(() => ({}), { cache: {} });
    const importFunctionSource = caller.replace(/\bimport\(/g, 'dynamicImport(');
    const importConfig = vm.runInNewContext(`${importFunctionSource}; requireOrImport`, {
      privateC2: context.bootstrap,
      __filename: commonPath,
      installTransformIfNeeded() {},
      fileIsModule: () => true,
      import_url2: { default: { pathToFileURL } },
      loaderChannel: false,
      nextTask() {},
      dynamicImport(specifier) { imported.push(specifier); return Promise.resolve({ loaded: specifier }); },
      require: importRequire,
      currentFileDepsCollector: () => undefined
    });
    await importConfig(configPath);
    assert.equal(imported.length, 1);
    assert.equal(fileURLToPath(imported[0]), configPath);
    const loaderImports = [];
    const preflightErrors = [];
    let nextTaskCalls = 0;
    const loaderChannelImportSource = caller.replace(/\bimport\(/g, 'dynamicImport(');
    const loaderChannelImport = vm.runInNewContext(`${loaderChannelImportSource}; requireOrImport`, {
      privateC2: context.bootstrap,
      __filename: commonPath,
      installTransformIfNeeded() {},
      fileIsModule: () => true,
      import_url2: { default: { pathToFileURL } },
      loaderChannel: true,
      nextTask() { nextTaskCalls += 1; },
      debugTest(...args) { preflightErrors.push(args.at(-1)); },
      dynamicImport(specifier) { loaderImports.push(specifier); return Promise.resolve({ loaded: specifier }); },
      require: importRequire,
      currentFileDepsCollector: () => undefined
    });
    await loaderChannelImport(configPath);
    assert.equal(preflightErrors.length, 1);
    assert.equal(preflightErrors[0]?.name, 'C2DynamicModuleRefusalError');
    assert.equal(preflightErrors[0]?.siteId, 'DS02');
    assert.equal(loaderImports.length, 1);
    assert.equal(fileURLToPath(loaderImports[0]), configPath);
    assert.equal(nextTaskCalls, 2);
    outcomes.push({ variant: 'import-config-esm', siteId: 'DS02', disposition: 'bound', invoked: true, resolvedTarget: path.relative(context.runtimeRoot, configPath), refusal: null, expansionGuards: ['DS02-ESM-import', 'DS02-ESM-preflight'], preflightRefusal: { siteId: preflightErrors[0].siteId, name: preflightErrors[0].name }, loaderImports: loaderImports.length, nextTaskCalls, effectsBeforeDecision: 0 });

    const commonTransform = extractGeneratedFunction(commonSource, 'function transformHook(', '\nfunction calculateHash');
    const commonBundleTarget = byId.get('DS01').approvedTargets[0];
    const commonRequire = createRequire(commonPath);
    const commonBundles = [];
    const generatedCommonRequire = Object.assign(specifier => {
      commonBundles.push(specifier);
      return commonRequire(specifier);
    }, { cache: commonRequire.cache });
    const commonTransformHook = vm.runInNewContext(`${commonTransform}; transformHook`, {
      privateC2: context.bootstrap,
      __filename: commonPath,
      process: { env: {} },
      transformData: new Map(),
      import_package3: { libPath: () => commonBundleTarget.specifier },
      calculateHash: () => 'dynamic-common-transform-test',
      getFromCompilationCache: () => ({ cachedCode: undefined, addToCache: (code, map) => ({ serializedCache: { code, map } }) }),
      _transformConfig: { babelPlugins: [], jsxImportSource: undefined },
      require: generatedCommonRequire
    });
    const commonTransformed = commonTransformHook('const commonSpecifier = "node:fs"; module.exports = require(commonSpecifier);', path.join(context.runtimeRoot, 'project/frontend/tests/browser/mobile-journeys.spec.ts'), null);
    assert.deepEqual(commonBundles, [commonBundleTarget.specifier]);
    assert.match(commonTransformed.serializedCache.code, /dynamicModule\("DS23"/);
    const commonOutputPath = path.join(context.runtimeRoot, 'project/frontend/c2-common-emitted-require.cjs');
    route.writeRetainedFile(commonOutputPath, commonTransformed.serializedCache.code, 'c2-common-emitted-require');
    const commonFs = createRequire(commonOutputPath)(commonOutputPath);
    assert.equal(typeof commonFs.readFileSync, 'function');
    const loaderName = 'frontend/node_modules/playwright/lib/transform/esmLoader.js';
    const loaderPath = path.join(context.runtimeRoot, 'project/frontend/node_modules/playwright/lib/transform/esmLoader.js');
    const loaderSource = context.generated[loaderName];
    const transformSource = extractGeneratedFunction(loaderSource, 'function transformHook(', '\nfunction calculateHash');
    const bundleTarget = byId.get('DS24').approvedTargets[0];
    const actualRequire = createRequire(loaderPath);
    const loadedBundles = [];
    const generatedRequire = Object.assign(specifier => {
      loadedBundles.push(specifier);
      return actualRequire(specifier);
    }, { cache: actualRequire.cache });
    const transformHook = vm.runInNewContext(`${transformSource}; transformHook`, {
      privateC2: context.bootstrap,
      __filename: loaderPath,
      process: { env: {} },
      transformData: new Map(),
      import_package2: { libPath: () => bundleTarget.specifier },
      _externalMatcher: () => false,
      belongsToNodeModules: () => false,
      calculateHash: () => 'dynamic-closure-test',
      getFromCompilationCache: () => ({ cachedCode: undefined, addToCache: (code, map) => ({ serializedCache: { code, map } }) }),
      _transformConfig: { babelPlugins: [], jsxImportSource: undefined },
      require: generatedRequire
    });
    const transformedRequire = transformHook('const specifier = "node:fs"; module.exports = require(specifier);', path.join(context.runtimeRoot, 'project/frontend/tests/browser/mobile-journeys.spec.ts'), null);
    assert.match(transformedRequire.serializedCache.code, /dynamicModule\("DS23"/);
    const transformedRequirePath = path.join(context.runtimeRoot, 'project/frontend/c2-babel-emitted-require.cjs');
    route.writeRetainedFile(transformedRequirePath, transformedRequire.serializedCache.code, 'c2-babel-emitted-require');
    const transformedFs = createRequire(transformedRequirePath)(transformedRequirePath);
    assert.equal(typeof transformedFs.readFileSync, 'function');
    const transformedImportPath = path.join(context.runtimeRoot, 'project/frontend/c2-babel-emitted-import.mjs');
    const transformedImport = transformHook('const specifier = "node:path"; export const loaded = await import(specifier);', path.join(context.runtimeRoot, 'project/frontend/tests/browser/mobile-journeys.spec.ts'), pathToFileURL(transformedImportPath));
    assert.match(transformedImport.serializedCache.code, /dynamicModule\("DS25"/);
    route.writeRetainedFile(transformedImportPath, transformedImport.serializedCache.code, 'c2-babel-emitted-import');
    const transformedPath = await import(pathToFileURL(transformedImportPath).href);
    assert.equal(typeof transformedPath.loaded.join, 'function');
    assert.deepEqual(loadedBundles, [bundleTarget.specifier, bundleTarget.specifier]);
    outcomes.push({ variant: 'transform-bundle-load', siteId: 'DS01+DS24', disposition: 'bound', invoked: true, resolvedTarget: [commonBundleTarget.resolvedPath, bundleTarget.resolvedPath], refusal: null, effectsBeforeDecision: 0 });

    const requireCallerPath = path.join(context.runtimeRoot, 'project/frontend/c2-emitted-require.cjs');
    const requireCaller = context.bootstrap.guardTransformedCode('const specifier = "node:fs"; module.exports = require(specifier);', requireCallerPath, false);
    route.writeRetainedFile(requireCallerPath, requireCaller, 'c2-emitted-require');
    const requiredValue = createRequire(requireCallerPath)(requireCallerPath);
    assert.equal(typeof requiredValue.readFileSync, 'function');
    outcomes.push({ variant: 'loader-emitted-require', siteId: 'DS23', disposition: 'bound', invoked: true, resolvedTarget: 'node:fs', refusal: null, effectsBeforeDecision: 0 });

    const importCallerPath = path.join(context.runtimeRoot, 'project/frontend/c2-emitted-import.mjs');
    const importCaller = context.bootstrap.guardTransformedCode('const specifier = "node:path"; export const loaded = await import(specifier);', importCallerPath, true);
    route.writeRetainedFile(importCallerPath, importCaller, 'c2-emitted-import');
    const importResult = await import(pathToFileURL(importCallerPath).href);
    assert.equal(typeof importResult.loaded.join, 'function');
    outcomes.push({ variant: 'loader-emitted-import', siteId: 'DS25', disposition: 'bound', invoked: true, resolvedTarget: 'node:path', refusal: null, effectsBeforeDecision: 0 });

    const packagePath = path.join(context.runtimeRoot, 'project/frontend/node_modules/playwright/lib/package.js');
    const packageModule = createRequire(packagePath)(packagePath);
    assert.equal(packageModule.packageJSON.name, 'playwright');
    outcomes.push({ variant: 'package-json-read', siteId: 'DS05', disposition: 'bound', invoked: true, resolvedTarget: 'project/frontend/node_modules/playwright/package.json', refusal: null, effectsBeforeDecision: 0 });

    const babelName = 'frontend/node_modules/playwright/lib/transform/babelBundle.js';
    const babelGuard = generatedSite('DS18');
    const sourceLine = babelGuard.lines[babelGuard.site.guardLine - 1];
    const requireOffset = sourceLine.indexOf('require(');
    assert.ok(requireOffset >= 0);
    const refusalCall = extractBalancedCall(sourceLine, requireOffset);
    const effects = { resolve: 0, require: 0 };
    const refusedRequire = Object.assign(() => { effects.require += 1; throw new Error('require must not run'); }, { resolve() { effects.resolve += 1; return '/stock/node_modules/config.js'; } });
    let stockError;
    try { vm.runInNewContext(refusalCall, { privateC2: context.bootstrap, __filename: path.join(context.runtimeRoot, babelName), require: refusedRequire, name: 'stock-config' }); } catch (error) { stockError = error; }
    assert.equal(stockError?.name, 'C2DynamicModuleRefusalError');
    assert.deepEqual(effects, { resolve: 0, require: 0 });
    outcomes.push({ variant: 'refused-stock-leak', siteId: 'DS18', disposition: 'refused', invoked: true, resolvedTarget: null, refusal: stockError.name, effectsBeforeDecision: 0 });

    const loaderGuard = generatedSite('DS23');
    const loaderLine = loaderGuard.lines[loaderGuard.site.guardLine - 1];
    const loaderOffset = loaderLine.indexOf('mod.require(');
    assert.ok(loaderOffset >= 0);
    const unapprovedCall = extractBalancedCall(loaderLine, loaderOffset);
    let moduleRequireCalls = 0;
    let computedError;
    try { vm.runInNewContext(`(function() { ${unapprovedCall} })()`, { privateC2: context.bootstrap, __filename: loaderGuard.filename, request: `${context.runtimeRoot}/project/frontend/node_modules/stock.js`, mod: { require() { moduleRequireCalls += 1; return {}; } } }); } catch (error) { computedError = error; }
    assert.equal(computedError?.name, 'C2DynamicModuleRefusalError');
    assert.equal(moduleRequireCalls, 0);
    const rejectedImportPath = path.join(context.runtimeRoot, 'project/frontend/c2-refused-import.mjs');
    const rejectedImport = context.bootstrap.guardTransformedCode(`const specifier = ${JSON.stringify(`${context.runtimeRoot}/project/frontend/node_modules/stock.js`)}; export const loaded = await import(specifier);`, rejectedImportPath, true);
    route.writeRetainedFile(rejectedImportPath, rejectedImport, 'c2-refused-import');
    let importError;
    try { await import(pathToFileURL(rejectedImportPath).href); } catch (error) { importError = error; }
    assert.equal(importError?.name, 'C2DynamicModuleRefusalError');
    outcomes.push({ variant: 'refused-unapproved-computed', siteId: 'DS23+DS25', disposition: 'bound', invoked: true, resolvedTarget: null, refusal: importError.name, effectsBeforeDecision: 0 });

    assert.equal(outcomes.length, callerVariants.length);
    assert.equal(route.nativeEffectCount(), 0);
    return { callers: outcomes };
  });

  const variants = results.flatMap(result => result.variants);
  const passed = results.length === 4 && results.every(result => result.status === 'passed' && result.nativeEffects === 0) && new Set(variants).size === variants.length;
  return { schema: 1, status: passed ? 'passed' : 'failed', expectedCases: 4, passedCases: results.filter(result => result.status === 'passed').length, failedCases: results.filter(result => result.status !== 'passed').length, cases: results, variants: variants.length, nativeEffects: results.reduce((total, result) => total + result.nativeEffects, 0) };
}
