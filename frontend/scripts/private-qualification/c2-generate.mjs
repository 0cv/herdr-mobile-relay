import { createHash } from 'node:crypto';
import { readFileSync, lstatSync } from 'node:fs';
import { createRequire, isBuiltin } from 'node:module';
import { isAbsolute, relative, resolve, dirname, join, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import { applyC2Edits, scanDynamicCalls, legacyDynamicExpression, justifiedDynamicSiteExpansions } from './c2-edits.mjs';

const scriptDirectory = dirname(fileURLToPath(import.meta.url));
const repositoryRoot = resolve(scriptDirectory, '../../..');
const expectedBaselineSHA256 = 'eb3aa68403869d3c9565be4dc3985910d7c3764d189556a2b5da82174b6b6db3';
const expectedC1RunnerSHA256 = 'f1ebbae7ab23305089c726578ad54be271ad46dc33b438121f7fd6f141f7c344';
const expectedC1SupportSHA256 = 'f124925c1ea3a35797fbe5e837a40296574970ad3bfe503df88354c5f6a4b169';
const expectedC1ResultSHA256 = '7b7db0e8a0d287f0976a4c3683a8f9429ea4187caed80e779e86d13f58e390c4';
const expectedC1SummarySHA256 = '9f4e55b1c75512bef3a2ab51c5f1119fb02e83fa8d670be5a7f7fc844d6e3727';
const projectInputs = [
  'frontend/package.json',
  'frontend/bun.lock',
  'frontend/playwright.config.ts',
  'frontend/tests/browser/mobile-journeys.spec.ts',
  'frontend/tests/browser/release-resources.spec.ts',
  'frontend/scripts/pages-policy.ts',
  'frontend/scripts/browser-server.mjs'
];
const patchedClosurePaths = new Set([
  'frontend/node_modules/@playwright/test/cli.js',
  'frontend/node_modules/playwright/lib/common/index.js',
  'frontend/node_modules/playwright/lib/runner/index.js',
  'frontend/node_modules/playwright/lib/worker/workerProcessEntry.js',
  'frontend/node_modules/playwright-core/lib/coreBundle.js'
]);

function sha256(value) {
  return createHash('sha256').update(value).digest('hex');
}

export function assertC2BudgetEnvelope(measured, limits, label = 'C2 output') {
  if (!Number.isSafeInteger(limits?.allocatedBytes) || limits.allocatedBytes < 0 || limits.entries !== undefined && (!Number.isSafeInteger(limits.entries) || limits.entries < 0) || limits.maxDepth !== undefined && (!Number.isSafeInteger(limits.maxDepth) || limits.maxDepth < 0))
    throw new Error(`C2 ${label} limits are malformed`);
  const entryCount = measured?.entryCount ?? measured?.entries;
  const maxDepth = measured?.maxDepth;
  if (!Number.isSafeInteger(measured?.allocatedBytes) || measured.allocatedBytes < 0 || limits.entries !== undefined && (!Number.isSafeInteger(entryCount) || entryCount < 0) || limits.maxDepth !== undefined && (!Number.isSafeInteger(maxDepth) || maxDepth < 0) || entryCount !== undefined && (!Number.isSafeInteger(entryCount) || entryCount < 0) || maxDepth !== undefined && (!Number.isSafeInteger(maxDepth) || maxDepth < 0))
    throw new Error(`C2 ${label} measurement is malformed`);
  const exceeded = measured.allocatedBytes > limits.allocatedBytes || limits.entries !== undefined && entryCount > limits.entries || limits.maxDepth !== undefined && maxDepth > limits.maxDepth;
  if (exceeded) throw new Error(`C2 ${label} exceeds its effective output envelope: ${JSON.stringify({ allocatedBytes: measured.allocatedBytes, entryCount, maxDepth, limits })}`);
  return { allocatedBytes: measured.allocatedBytes, ...(entryCount !== undefined ? { entryCount } : {}), ...(maxDepth !== undefined ? { maxDepth } : {}), basis: 'allocatedBytes', limits: { ...limits } };
}

function text(bytes, label) {
  const value = bytes.toString('utf8');
  if (!Buffer.from(value, 'utf8').equals(bytes)) throw new Error(`Source is not UTF-8 text: ${label}`);
  return value;
}

function readBounded(path, identity, root) {
  const absolute = resolve(root, path);
  const stat = lstatSync(absolute);
  if (!stat.isFile() || stat.isSymbolicLink() || (identity.bytes !== undefined && stat.size !== identity.bytes) || (identity.dev !== undefined && String(stat.dev) !== identity.dev) || (identity.ino !== undefined && String(stat.ino) !== identity.ino))
    throw new Error(`C2 source type or size changed: ${path}`);
  const bytes = readFileSync(absolute);
  if (sha256(bytes) !== identity.sha256)
    throw new Error(`C2 source digest changed: ${path}`);
  return bytes;
}

function c1Input(baseline) {
  const runner = baseline.reusableC1.find(item => item.path.endsWith('/revision-06/runner.derived.cjs'));
  const support = baseline.reusableC1.find(item => item.path.endsWith('/revision-06/privateCompletion.cjs'));
  const completion = baseline.reusableC1.find(item => item.path.endsWith('/completion-result.json'));
  const summary = baseline.reusableC1.find(item => item.path.endsWith('/attempt-06.summary.json'));
  if (!runner || runner.sha256 !== expectedC1RunnerSHA256 || !support || support.sha256 !== expectedC1SupportSHA256 || !completion || completion.sha256 !== expectedC1ResultSHA256 || !summary || summary.sha256 !== expectedC1SummarySHA256)
    throw new Error('Passed C1 source/evidence bindings do not match the approved C2 inputs');
  const runnerBytes = readFileSync(runner.path);
  const supportBytes = readFileSync(support.path);
  const completionValue = JSON.parse(readFileSync(completion.path, 'utf8'));
  const summaryValue = JSON.parse(readFileSync(summary.path, 'utf8'));
  if (sha256(runnerBytes) !== expectedC1RunnerSHA256 || sha256(supportBytes) !== expectedC1SupportSHA256 || completionValue.status !== 'passed' || completionValue.sourceTests?.cases !== 79 || completionValue.sourceTests?.passed !== 79 || completionValue.sourceTests?.variants !== 80 || summaryValue.cases !== 79 || summaryValue.passed !== 79 || !Array.isArray(summaryValue.failures) || summaryValue.failures.length !== 0 || summaryValue.derivedSHA256 !== expectedC1RunnerSHA256 || summaryValue.supportSHA256 !== expectedC1SupportSHA256)
    throw new Error('Passed 3ea2ca12 evidence is not the recorded 79/79, 80-variant result');
  return { runner: text(runnerBytes, runner.path), runnerIdentity: runner, support: text(supportBytes, support.path), supportIdentity: support, completionIdentity: completion, summaryIdentity: summary };
}

function readInputs(baseline, c1) {
  if (baseline.repository !== repositoryRoot) throw new Error('C2 baseline repository root differs');
  const inputIdentities = new Map(baseline.inputs.map(item => [item.path, item]));
  const original = {};
  const inputReceipts = [];
  for (const path of projectInputs) {
    const identity = inputIdentities.get(path);
    if (!identity) throw new Error(`C2 baseline omitted project input: ${path}`);
    const bytes = readBounded(path, identity, repositoryRoot);
    original[path] = text(bytes, path);
    inputReceipts.push({ path, sha256: identity.sha256, bytes: identity.bytes, lines: identity.lines });
  }
  const supportIdentity = baseline.qualificationSources.find(item => item.path.endsWith('/privateCompletion.cjs'));
  if (!supportIdentity) throw new Error('C2 baseline omitted the unchanged C1 support source');
  const supportBytes = readBounded('frontend/scripts/private-qualification/privateCompletion.cjs', supportIdentity, repositoryRoot);
  if (sha256(supportBytes) !== c1.supportIdentity.sha256 || !supportBytes.equals(Buffer.from(c1.support)))
    throw new Error('Repository C1 completion support changed and cannot be reused');
  const closureOriginal = {};
  for (const source of baseline.closureCandidate) {
    if (source.path.includes('/node_modules/playwright/node_modules/fsevents/')) continue;
    const bytes = readBounded(source.path, source, repositoryRoot);
    closureOriginal[source.path] = bytes;
  }
  return { original, closureOriginal, inputReceipts };
}

function destinationForSource(sourcePath, runtimeRoot) {
  if (sourcePath.startsWith('frontend/node_modules/'))
    return join(runtimeRoot, 'project/frontend/node_modules', sourcePath.slice('frontend/node_modules/'.length));
  if (sourcePath.startsWith('frontend/'))
    return join(runtimeRoot, 'project/frontend', sourcePath.slice('frontend/'.length));
  throw new Error(`C2 source path is outside the supported project closure: ${sourcePath}`);
}

function selectedOriginals(closureOriginal, projectOriginal, c1, dynamicSites = []) {
  const sources = {};
  const paths = new Set([...patchedClosurePaths, ...dynamicSites.map(site => site.from)]);
  for (const path of paths) {
    const bytes = closureOriginal[path];
    if (!bytes) throw new Error(`C2 selected vendor source is unbound: ${path}`);
    sources[path] = path === 'frontend/node_modules/playwright/lib/runner/index.js' ? c1.runner : text(bytes, path);
  }
  const releaseFixture = projectOriginal['frontend/tests/browser/release-resources.spec.ts'];
  return { sources, releaseFixture };
}

function codeMask(source) {
  const mask = new Uint8Array(source.length);
  const regexPreceders = new Set(['', '(', '[', '{', ',', ':', ';', '=', '!', '?', '&&', '||', '??', '=>', 'return', 'case', 'throw', 'delete', 'void', 'typeof', 'instanceof', 'in', 'of', 'yield', 'await']);
  let previous = '';
  for (let index = 0; index < source.length;) {
    const character = source[index];
    const next = source[index + 1];
    if (character === '/' && next === '/') {
      while (index < source.length && source[index] !== '\n') index += 1;
      continue;
    }
    if (character === '/' && next === '*') {
      index += 2;
      while (index < source.length && !(source[index] === '*' && source[index + 1] === '/')) index += 1;
      index = Math.min(source.length, index + 2);
      continue;
    }
    if (character === "'" || character === '"' || character === '`') {
      const quote = character;
      index += 1;
      while (index < source.length) {
        if (source[index] === '\\') index += 2;
        else if (source[index++] === quote) break;
      }
      previous = 'literal';
      continue;
    }
    if (character === '/' && regexPreceders.has(previous)) {
      index += 1;
      let inClass = false;
      while (index < source.length) {
        if (source[index] === '\\') index += 2;
        else if (source[index] === '[') { inClass = true; index += 1; }
        else if (source[index] === ']' && inClass) { inClass = false; index += 1; }
        else if (source[index++] === '/' && !inClass) break;
      }
      while (/[a-z]/i.test(source[index] || '')) index += 1;
      previous = 'literal';
      continue;
    }
    if (/[a-z_$]/i.test(character)) {
      const start = index;
      while (/[\w$]/.test(source[index] || '')) {
        mask[index] = 1;
        index += 1;
      }
      previous = source.slice(start, index);
      continue;
    }
    if (/\s/.test(character)) {
      index += 1;
      continue;
    }
    mask[index] = 1;
    if (character === '&' && next === '&' || character === '|' && next === '|' || character === '?' && next === '?' || character === '=' && next === '>') {
      mask[index + 1] = 1;
      previous = source.slice(index, index + 2);
      index += 2;
    } else {
      previous = character;
      index += 1;
    }
  }
  return mask;
}

export function scanDynamicSites(sourcePath, source) {
  return scanDynamicCalls(source).map(match => ({
    from: sourcePath,
    expression: match.legacyExpression,
    callExpression: match.expression,
    kind: match.kind,
    line: match.line,
    sourceOffset: match.start,
    openOffset: match.open,
    closeOffset: match.close,
    expressionStart: match.expressionStart,
    classification: 'refused-before-program-effects-until-explicitly-bound'
  }));
}

export function assertDynamicSiteScanClosed(scannedSites, allowedSites, options = {}) {
  if (!Array.isArray(scannedSites) || !Array.isArray(allowedSites)) throw new Error('Dynamic source scan and inventory must be arrays');
  const remaining = new Map();
  function key(site, matchLine) {
    const digest = site.expressionSHA256 || (typeof site.expression === 'string' ? sha256(Buffer.from(site.expression)) : null);
    if (typeof site.from !== 'string' || !digest || !Number.isSafeInteger(site.line) || site.line < 1)
      throw new Error('Dynamic source scan or inventory entry is malformed');
    return `${site.from}\0${matchLine ? `${site.line}\0` : ''}${digest}`;
  }
  for (const site of allowedSites) {
    const identity = key(site, options.matchLines === true);
    if (!remaining.has(identity)) remaining.set(identity, []);
    remaining.get(identity).push(site);
  }
  const matches = [];
  for (const site of scannedSites) {
    const identity = key(site, options.matchLines === true);
    const candidate = remaining.get(identity)?.shift();
    if (!candidate) throw new Error(`Generated dynamic site is absent from the frozen inventory: ${site.from}:${site.line}`);
    matches.push({ siteId: candidate.siteId, from: site.from, line: site.line, expressionSHA256: identity.split('\0').at(-1) });
  }
  const unscannedSiteIds = [...remaining.values()].flat().map(site => site.siteId);
  if (options.requireComplete === true && unscannedSiteIds.length)
    throw new Error(`Dynamic source scan omitted frozen inventory sites: ${unscannedSiteIds.join(', ')}`);
  return { scannedSites: scannedSites.length, inventorySites: allowedSites.length, matchedSites: matches.length, unscannedSiteIds, matches };
}

export function assertOriginalDynamicSiteScanClosed(scannedSites, inventorySites, originalSources) {
  if (!Array.isArray(scannedSites) || !Array.isArray(inventorySites) || !originalSources)
    throw new Error('Original dynamic scan inputs are malformed');
  const inventoryGroups = new Map();
  const scannedGroups = new Map();
  const sourceHashes = new Map();
  const groupKey = (from, expressionSHA256) => `${from}\0${expressionSHA256}`;
  for (const [index, site] of inventorySites.entries()) {
    if (typeof site.from !== 'string' || !site.from.startsWith('frontend/') || site.from.startsWith('/') || site.from.split('/').includes('..') || !Number.isSafeInteger(site.line) || site.line < 1 || !/^[a-f0-9]{64}$/.test(site.expressionSHA256 || '') || !/^[a-f0-9]{64}$/.test(site.originalFileSHA256 || ''))
      throw new Error('Frozen original dynamic-site identity is malformed');
    if (typeof site.expression === 'string' && sha256(Buffer.from(site.expression)) !== site.expressionSHA256)
      throw new Error(`Frozen dynamic-site expression changed: ${site.siteId}`);
    const source = originalSources[site.from];
    if (source === undefined) throw new Error(`Frozen dynamic-site source is missing: ${site.from}`);
    const sourceDigest = sha256(Buffer.isBuffer(source) ? source : Buffer.from(source));
    if (sourceDigest !== site.originalFileSHA256)
      throw new Error(`Frozen original source hash changed: ${site.from}`);
    const previousHash = sourceHashes.get(site.from);
    if (previousHash && previousHash !== site.originalFileSHA256)
      throw new Error(`Frozen original source hashes disagree: ${site.from}`);
    sourceHashes.set(site.from, site.originalFileSHA256);
    const key = groupKey(site.from, site.expressionSHA256);
    if (!inventoryGroups.has(key)) inventoryGroups.set(key, []);
    inventoryGroups.get(key).push({ site, index });
  }
  for (const [index, site] of scannedSites.entries()) {
    if (typeof site.from !== 'string' || typeof site.expression !== 'string' || !Number.isSafeInteger(site.line) || site.line < 1)
      throw new Error('Original dynamic source scan entry is malformed');
    const expressionSHA256 = sha256(Buffer.from(site.expression));
    const key = groupKey(site.from, expressionSHA256);
    if (!inventoryGroups.has(key))
      throw new Error(`Original dynamic site is absent from the frozen inventory: ${site.from}:${site.line}`);
    if (!scannedGroups.has(key)) scannedGroups.set(key, []);
    scannedGroups.get(key).push({ site, index, expressionSHA256 });
  }
  const matches = [];
  const groups = [];
  for (const [key, inventoryGroup] of inventoryGroups) {
    const scannedGroup = scannedGroups.get(key) || [];
    const [from, expressionSHA256] = key.split('\0');
    if (inventoryGroup.length !== scannedGroup.length)
      throw new Error(`Original dynamic-site occurrence count differs from the frozen inventory: ${from}:${expressionSHA256} expected ${inventoryGroup.length}, found ${scannedGroup.length}`);
    const inventoryOrdered = [...inventoryGroup].sort((left, right) => left.site.line - right.site.line || left.index - right.index);
    const scannedOrdered = [...scannedGroup].sort((left, right) => left.site.line - right.site.line || (left.site.sourceOffset ?? left.index) - (right.site.sourceOffset ?? right.index) || left.index - right.index);
    const mappingUnambiguous = scannedOrdered.every((item, occurrence, ordered) => Number.isSafeInteger(item.site.sourceOffset) && (occurrence === 0 || ordered[occurrence - 1].site.sourceOffset < item.site.sourceOffset));
    const groupMatches = inventoryOrdered.map(({ site }, occurrence) => {
      const scanned = scannedOrdered[occurrence].site;
      return {
        siteId: site.siteId,
        from,
        expressionSHA256,
        inventoryCoordinate: { path: site.from, line: site.line, representation: 'frozen-transformed-source' },
        originalCoordinate: { path: scanned.from, line: scanned.line, sourceOffset: scanned.sourceOffset, occurrence, ...(scanned.kind ? { kind: scanned.kind } : {}), representation: 'raw-original-source' },
        occurrenceMapping: { strategy: 'occurrence-ordinal-by-exact-path-expression-and-full-source-hash', unambiguous: mappingUnambiguous }
      };
    });
    matches.push(...groupMatches);
    groups.push({ from, expressionSHA256, occurrences: groupMatches.length, mappingUnambiguous });
  }
  const scannedOnlyGroups = [...scannedGroups.keys()].filter(key => !inventoryGroups.has(key));
  if (scannedOnlyGroups.length)
    throw new Error(`Original dynamic scan found unlisted source/expression identities: ${scannedOnlyGroups.join(', ')}`);
  return {
    scannedSites: scannedSites.length,
    inventorySites: inventorySites.length,
    matchedSites: matches.length,
    unscannedSiteIds: [],
    sourceFiles: [...sourceHashes].map(([from, originalFileSHA256]) => ({ from, originalFileSHA256 })).sort((left, right) => left.from.localeCompare(right.from)),
    groups,
    matches
  };
}

export function assertOriginalDynamicSiteSemanticsUnambiguous(originalScan, dynamicSiteRecords) {
  if (!originalScan || !Array.isArray(originalScan.matches) || !Array.isArray(dynamicSiteRecords))
    throw new Error('Original dynamic-site semantic mapping inputs are malformed');
  const records = new Map(dynamicSiteRecords.map(site => [site.siteId, site]));
  if (records.size !== dynamicSiteRecords.length) throw new Error('Generated dynamic-site IDs are duplicated');
  const duplicateGroups = new Map();
  for (const match of originalScan.matches) {
    const key = `${match.from}\0${match.expressionSHA256}`;
    if (!duplicateGroups.has(key)) duplicateGroups.set(key, []);
    duplicateGroups.get(key).push(match);
  }
  const checked = [];
  for (const matches of duplicateGroups.values()) {
    if (matches.length < 2) continue;
    const semantics = matches.map(match => {
      const record = records.get(match.siteId);
      if (!record) throw new Error(`Generated dynamic-site semantics are missing: ${match.siteId}`);
      return JSON.stringify({
        kind: match.originalCoordinate.kind || null,
        disposition: record.disposition,
        approvedTargets: record.approvedTargets,
        enforcedBy: record.enforcedBy,
        refusal: record.refusal || null
      });
    });
    if (new Set(semantics).size > 1 && matches.some(match => !match.occurrenceMapping.unambiguous))
      throw new Error(`Ambiguous duplicate dynamic sites have different guard or disposition semantics: ${matches.map(match => match.siteId).join(', ')}`);
    checked.push({ from: matches[0].from, expressionSHA256: matches[0].expressionSHA256, siteIds: matches.map(match => match.siteId), mappingUnambiguous: matches.every(match => match.occurrenceMapping.unambiguous), semanticsDiffer: new Set(semantics).size > 1 });
  }
  return { duplicateGroups: checked };
}

function maskedCallEnd(source, open, mask, label) {
  let depth = 0;
  for (let index = open; index < source.length; index += 1) {
    if (!mask[index]) continue;
    if (source[index] === '(') depth += 1;
    else if (source[index] === ')' && --depth === 0) return index;
  }
  throw new Error(`Generated ${label} call is unterminated`);
}

function callArguments(source, open, close, mask) {
  const boundaries = [];
  let parentheses = 0;
  let brackets = 0;
  let braces = 0;
  for (let index = open + 1; index < close; index += 1) {
    if (!mask[index]) continue;
    const character = source[index];
    if (character === '(') parentheses += 1;
    else if (character === ')') parentheses -= 1;
    else if (character === '[') brackets += 1;
    else if (character === ']') brackets -= 1;
    else if (character === '{') braces += 1;
    else if (character === '}') braces -= 1;
    else if (character === ',' && parentheses === 0 && brackets === 0 && braces === 0) boundaries.push(index);
  }
  const starts = [open + 1, ...boundaries.map(index => index + 1)];
  const ends = [...boundaries, close];
  return starts.map((start, index) => source.slice(start, ends[index]).trim());
}

function scanNamedCalls(source, pattern, label) {
  const mask = codeMask(source);
  const calls = [];
  pattern.lastIndex = 0;
  for (const match of source.matchAll(pattern)) {
    if (!mask[match.index]) continue;
    const open = source.indexOf('(', match.index);
    const close = maskedCallEnd(source, open, mask, label);
    calls.push({ start: match.index, open, close, line: source.slice(0, match.index).split('\n').length, args: callArguments(source, open, close, mask) });
  }
  return calls;
}

function literalArgument(source) {
  const match = /^\s*(["'])([\s\S]*)\1\s*$/.exec(source);
  return match ? match[2] : null;
}

function unwrappedArrowExpression(source) {
  const match = /^\s*\(\s*\)\s*=>\s*([\s\S]+)$/.exec(source);
  if (!match) return null;
  let expression = match[1].trim();
  if (expression.startsWith('(') && expression.endsWith(')')) {
    const mask = codeMask(expression);
    const close = maskedCallEnd(expression, 0, mask, 'deferred expression');
    if (close === expression.length - 1) expression = expression.slice(1, -1).trim();
  }
  return expression;
}

function generatedDynamicGuards(source, sourcePath) {
  const moduleCalls = scanNamedCalls(source, /\bprivateC2\s*\.\s*dynamicModule\s*\(/g, 'dynamicModule');
  const assetCalls = scanNamedCalls(source, /\b__c2DynamicAsset\s*\(/g, 'asset');
  const delegationCalls = scanNamedCalls(source, /\bprivateC2\s*\.\s*enforceDynamicSite\s*\(/g, 'delegation');
  return [
    ...moduleCalls.map(call => ({ ...call, sourcePath, style: 'privateC2.dynamicModule', siteId: literalArgument(call.args[0]), kind: literalArgument(call.args[1]), expression: unwrappedArrowExpression(call.args[2]) })),
    ...assetCalls.map(call => ({ ...call, sourcePath, style: '__c2DynamicAsset', siteId: literalArgument(call.args[0]), kind: 'import', expression: call.args[1] })),
    ...delegationCalls.map(call => ({ ...call, sourcePath, style: 'privateC2.enforceDynamicSite', siteId: literalArgument(call.args[0]), delegatedTo: call.args[1] }))
  ];
}

export function assertGeneratedDynamicScanClosed(scannedSites, dynamicSiteRecords, generatedSources) {
  if (!Array.isArray(scannedSites) || !Array.isArray(dynamicSiteRecords) || !generatedSources)
    throw new Error('Generated dynamic scan inputs are malformed');
  const guardedSites = dynamicSiteRecords.filter(site => site.enforcedBy.length === 0);
  const delegatedSites = dynamicSiteRecords.filter(site => site.enforcedBy.length > 0);
  const siteById = new Map(dynamicSiteRecords.map(site => [site.siteId, site]));
  if (siteById.size !== dynamicSiteRecords.length) throw new Error('Generated dynamic guard IDs are duplicated');
  const expectedGuards = [];
  for (const site of dynamicSiteRecords) {
    const source = generatedSources[site.from];
    if (typeof source !== 'string' || site.generatedSHA256 && sha256(Buffer.from(source)) !== site.generatedSHA256)
      throw new Error(`Generated dynamic guard source changed: ${site.siteId}`);
    if (typeof site.generatedFile !== 'string' || !Number.isSafeInteger(site.guardLine) || site.guardLine < 1)
      throw new Error(`Generated dynamic guard coordinate is malformed: ${site.siteId}`);
  }
  for (const site of guardedSites) {
    expectedGuards.push({
      siteId: site.siteId,
      from: site.from,
      kind: site.originalCoordinate?.kind,
      expressionSHA256: site.expressionSHA256,
      provenance: { type: 'frozen-inventory-occurrence', originalCoordinate: site.originalCoordinate }
    });
    for (const expansion of site.generatedExpansions || [])
      expectedGuards.push({ siteId: site.siteId, from: site.from, kind: expansion.kind, expressionSHA256: expansion.generatedExpressionSHA256, provenance: { type: 'justified-expansion', expansionId: expansion.expansionId, originalCoordinate: expansion.originalCoordinate, justification: expansion.justification } });
  }
  const actualGuards = [];
  const delegatedGuards = [];
  for (const [sourcePath, source] of Object.entries(generatedSources)) {
    if (typeof source !== 'string') continue;
    actualGuards.push(...generatedDynamicGuards(source, sourcePath).filter(call => call.style !== 'privateC2.enforceDynamicSite'));
    delegatedGuards.push(...generatedDynamicGuards(source, sourcePath).filter(call => call.style === 'privateC2.enforceDynamicSite'));
  }
  const actualCalls = Object.entries(generatedSources).flatMap(([sourcePath, source]) => typeof source === 'string' ? scanDynamicSites(sourcePath, source) : []);
  const scannedKeys = new Set(scannedSites.map(site => `${site.from}\0${site.sourceOffset}\0${site.kind}`));
  const actualKeys = new Set(actualCalls.map(site => `${site.from}\0${site.sourceOffset}\0${site.kind}`));
  if (scannedKeys.size !== scannedSites.length || actualKeys.size !== actualCalls.length || scannedKeys.size !== actualKeys.size || [...actualKeys].some(key => !scannedKeys.has(key)))
    throw new Error('Generated dynamic scanner omitted or duplicated a balanced loader occurrence');
  const guardUseCounts = new Map(actualGuards.map((guard, index) => [index, 0]));
  const expectedCounts = new Map(expectedGuards.map((expected, index) => [index, 0]));
  const delegatedCallGroups = new Map();
  for (const site of delegatedSites) {
    const key = JSON.stringify([site.from, site.originalCoordinate?.kind, site.expressionSHA256]);
    const group = delegatedCallGroups.get(key) || [];
    group.push(site);
    delegatedCallGroups.set(key, group);
  }
  for (const group of delegatedCallGroups.values()) group.sort((left, right) => (left.originalCoordinate?.sourceOffset ?? 0) - (right.originalCoordinate?.sourceOffset ?? 0));
  const delegatedCallOffsets = new Map();
  const generatedDelegatedCalls = [];
  const generatedOccurrences = [];
  const residualSites = [];
  for (const site of scannedSites) {
    const source = generatedSources[site.from];
    const expressionStart = site.expressionStart;
    if (typeof source !== 'string' || !Number.isSafeInteger(site.sourceOffset) || !Number.isSafeInteger(site.closeOffset) || !Number.isSafeInteger(expressionStart))
      throw new Error(`Generated dynamic scan coordinate is malformed: ${site.from}:${site.line}`);
    const direct = actualGuards.map((guard, index) => ({ guard, index })).filter(({ guard }) => guard.sourcePath === site.from && guard.start >= expressionStart && guard.close < site.closeOffset && source.slice(expressionStart, guard.start).trim() === '' && source.slice(guard.close + 1, site.closeOffset).trim() === '');
    if (!direct.length) {
      const key = JSON.stringify([site.from, site.kind, sha256(Buffer.from(site.expression))]);
      const group = delegatedCallGroups.get(key);
      const occurrence = delegatedCallOffsets.get(key) || 0;
      const delegated = group?.[occurrence];
      if (delegated) {
        delegatedCallOffsets.set(key, occurrence + 1);
        const guards = delegatedGuards.filter(guard => guard.siteId === delegated.siteId && guard.sourcePath === site.from);
        const guard = guards[0];
        const beforeCall = guard && guard.close < site.sourceOffset && source.slice(guard.close + 1, site.sourceOffset).trim() === '';
        const firstMethodStatement = guard && guard.start > site.closeOffset && source.slice(site.closeOffset + 1, guard.start).trim() === '{';
        if (guards.length !== 1 || !beforeCall && !firstMethodStatement)
          throw new Error(`Generated delegated loader guard is not adjacent to its original builder call: ${delegated.siteId}`);
        generatedDelegatedCalls.push({ siteId: delegated.siteId, from: site.from, line: site.line, sourceOffset: site.sourceOffset, guardStart: guards[0].start, enforcedBy: delegated.enforcedBy });
        continue;
      }
      residualSites.push(site);
      continue;
    }
    if (direct.length !== 1) throw new Error(`Generated dynamic call has multiple inner guards: ${site.from}:${site.line}`);
    const { guard, index: guardIndex } = direct[0];
    const siteRecord = siteById.get(guard.siteId);
    if (!siteRecord || siteRecord.from !== site.from || guard.kind !== site.kind)
      throw new Error(`Generated outer dynamic call has a wrong or unbound inner guard ID: ${guard.siteId || 'missing'}`);
    if (guard.style === 'privateC2.dynamicModule' && guard.args.length !== 4)
      throw new Error(`Generated dynamicModule guard has the wrong argument count: ${guard.siteId}`);
    if (guard.style === '__c2DynamicAsset' && guard.args.length !== 2)
      throw new Error(`Generated asset guard has the wrong argument count: ${guard.siteId}`);
    if (typeof guard.expression !== 'string') throw new Error(`Generated dynamic guard has no deferred expression: ${guard.siteId}`);
    const expressionSHA256 = guard.style === 'privateC2.dynamicModule'
      ? sha256(Buffer.from(legacyDynamicExpression(guard.expression)))
      : sha256(Buffer.from(guard.expression.trim()));
    const expectedIndex = expectedGuards.findIndex(expected => expected.siteId === guard.siteId && expected.from === site.from && expected.kind === guard.kind && expected.expressionSHA256 === expressionSHA256);
    if (expectedIndex < 0) throw new Error(`Generated dynamic guard ID or expression does not match its frozen provenance: ${guard.siteId}`);
    expectedCounts.set(expectedIndex, expectedCounts.get(expectedIndex) + 1);
    guardUseCounts.set(guardIndex, guardUseCounts.get(guardIndex) + 1);
    const expected = expectedGuards[expectedIndex];
    generatedOccurrences.push({
      siteId: guard.siteId,
      from: site.from,
      line: guard.line,
      outerKind: site.kind,
      guardKind: guard.kind,
      outerCallStart: site.sourceOffset,
      guardStart: guard.start,
      guardStyle: guard.style,
      expressionSHA256,
      generatedGuardCoordinate: { file: siteRecord.generatedFile, line: guard.line, representation: 'generated-private-source-guard' },
      generatedExpressionCoordinate: { file: siteRecord.generatedFile, line: site.line, representation: 'generated-private-source-dynamic-expression' },
      provenance: expected.provenance
    });
  }
  for (const [key, group] of delegatedCallGroups)
    if ((delegatedCallOffsets.get(key) || 0) !== group.length) throw new Error(`Generated delegated loader mapping omitted inventory occurrences: ${group.map(site => site.siteId).join(', ')}`);
  if (residualSites.length) assertDynamicSiteScanClosed(residualSites, [], { requireComplete: true });
  const unusedGuards = [...guardUseCounts].filter(([, count]) => count !== 1).map(([index]) => actualGuards[index].siteId || 'missing');
  if (unusedGuards.length) throw new Error(`Generated dynamic guard has no unique outer loader: ${unusedGuards.join(', ')}`);
  const missingGuards = [...expectedCounts].filter(([, count]) => count !== 1).map(([index]) => expectedGuards[index].siteId);
  if (missingGuards.length) throw new Error(`Generated dynamic scan omitted guarded inventory provenance: ${missingGuards.join(', ')}`);
  const duplicateGroups = new Map();
  for (const site of guardedSites) {
    const key = JSON.stringify([site.from, site.originalCoordinate?.kind, site.expressionSHA256]);
    const group = duplicateGroups.get(key) || [];
    group.push(site);
    duplicateGroups.set(key, group);
  }
  for (const group of duplicateGroups.values()) {
    if (group.length < 2) continue;
    const expectedIds = group.slice().sort((left, right) => (left.originalCoordinate?.sourceOffset ?? left.originalCoordinate?.occurrence ?? left.inventoryCoordinate?.line ?? 0) - (right.originalCoordinate?.sourceOffset ?? right.originalCoordinate?.occurrence ?? right.inventoryCoordinate?.line ?? 0)).map(site => site.siteId);
    const ids = new Set(group.map(site => site.siteId));
    const actualIds = generatedOccurrences.filter(item => ids.has(item.siteId) && item.provenance.type === 'frozen-inventory-occurrence').sort((left, right) => left.outerCallStart - right.outerCallStart).map(item => item.siteId);
    if (JSON.stringify(actualIds) !== JSON.stringify(expectedIds)) throw new Error(`Generated duplicate dynamic guard IDs were reordered: ${expectedIds.join(', ')}`);
  }
  for (const site of delegatedSites) {
    const matches = delegatedGuards.filter(guard => guard.siteId === site.siteId);
    const target = matches[0]?.delegatedTo;
    if (matches.length !== 1 || matches[0].sourcePath !== site.from || !Array.isArray(site.enforcedBy) || JSON.stringify(JSON.parse(target || 'null')) !== JSON.stringify(site.enforcedBy))
      throw new Error(`Generated delegated dynamic guard is missing, extra, or mismatched: ${site.siteId}`);
  }
  const unexpectedDelegations = delegatedGuards.filter(guard => !delegatedSites.some(site => site.siteId === guard.siteId));
  if (unexpectedDelegations.length) throw new Error(`Generated source contains an unbound delegated dynamic guard: ${unexpectedDelegations.map(guard => guard.siteId || 'missing').join(', ')}`);
  for (const site of dynamicSiteRecords) {
    const occurrences = generatedOccurrences.filter(item => item.siteId === site.siteId);
    const primary = occurrences.find(item => item.provenance.type === 'frozen-inventory-occurrence');
    if (primary) {
      site.generatedCoordinate = primary.generatedGuardCoordinate;
      site.generatedExpressionCoordinate = primary.generatedExpressionCoordinate;
      site.guardLine = primary.line;
    }
    site.generatedGuardOccurrences = occurrences.map(({ provenance, ...item }) => ({ ...item, provenance }));
    for (const expansion of site.generatedExpansions || []) {
      const mapped = occurrences.find(item => item.provenance.type === 'justified-expansion' && item.provenance.expansionId === expansion.expansionId);
      if (!mapped) throw new Error(`Generated dynamic expansion is not mapped to a guard: ${expansion.expansionId}`);
      expansion.generatedCoordinate = mapped.generatedGuardCoordinate;
      expansion.generatedExpressionCoordinate = mapped.generatedExpressionCoordinate;
    }
  }
  return {
    scannedSites: scannedSites.length,
    inventorySites: dynamicSiteRecords.length,
    matchedSites: generatedOccurrences.length,
    unscannedSiteIds: [],
    generatedGuards: generatedOccurrences,
    generatedDelegatedCalls,
    generatedDelegations: delegatedGuards.map(guard => ({ siteId: guard.siteId, sourcePath: guard.sourcePath, line: guard.line, enforcedBy: JSON.parse(guard.delegatedTo) })),
    generatedResidualSites: residualSites.length
  };
}

export function moduleEdges(sourcePath, source, privateSourcePath, privateRoot, closureTargets) {
  const edges = [];
  const mask = codeMask(source);
  const staticPatterns = [
    { kind: 'require', regex: /\brequire\s*\(\s*(['"])([^'"]+)\1\s*\)/g, group: 2 },
    { kind: 'import', regex: /\bimport\s*\(\s*(['"])([^'"]+)\1\s*\)/g, group: 2 },
    { kind: 'import-from', regex: /\b(?:import|export)\s+(?:[^'";]*?\s+from\s*)?(['"])([^'"]+)\1/g, group: 2 }
  ];
  const requireFromPrivate = createRequire(privateSourcePath);
  for (const pattern of staticPatterns) {
    for (const match of source.matchAll(pattern.regex)) {
      if (!mask[match.index]) continue;
      const specifier = match[pattern.group];
      const line = source.slice(0, match.index).split('\n').length;
      if (specifier === 'fsevents' || specifier.includes('/fsevents/')) {
        edges.push({ from: sourcePath, specifier, kind: pattern.kind, line, target: null, classification: 'optional-watch-excluded-with-refusal' });
        continue;
      }
      const optionalNativeFallbacks = { bufferutil: 'WS_NO_BUFFER_UTIL', 'utf-8-validate': 'WS_NO_UTF_8_VALIDATE' };
      if (optionalNativeFallbacks[specifier]) {
        const at = source.indexOf(`require("${specifier}")`);
        const evidence = source.slice(Math.max(0, at - 400), at + 800);
        if (!sourcePath.endsWith('/playwright-core/lib/utilsBundle.js') || at < 0 || !evidence.includes(optionalNativeFallbacks[specifier]) || !evidence.includes('catch'))
          throw new Error(`Optional native addon does not have a bound caught fallback: ${sourcePath}:${line} ${specifier}`);
        edges.push({ from: sourcePath, specifier, kind: pattern.kind, line, target: null, classification: 'optional-missing-native-addon-fallback' });
        continue;
      }
      if (specifier === 'electron' || specifier === 'electron/index.js') {
        if (!sourcePath.includes('/playwright-core/lib/server/electron/') && !sourcePath.endsWith('/playwright-core/lib/coreBundle.js'))
          throw new Error(`Unbound Electron entry is outside its refused lazy feature: ${sourcePath}:${line}`);
        edges.push({ from: sourcePath, specifier, kind: pattern.kind, line, target: null, classification: 'unsupported-lazy-electron-feature-refused' });
        continue;
      }
      if (specifier === 'kerberos') {
        if (!sourcePath.endsWith('/playwright-core/lib/utilsBundle.js') || !source.includes('Negotiate proxy authentication') || !source.includes('The "kerberos" package is required'))
          throw new Error(`Unbound Kerberos edge is outside its refused proxy-authentication feature: ${sourcePath}:${line}`);
        edges.push({ from: sourcePath, specifier, kind: pattern.kind, line, target: null, classification: 'unsupported-proxy-auth-refused' });
        continue;
      }
      let target;
      if (specifier.startsWith('node:') || isBuiltin(specifier)) {
        edges.push({ from: sourcePath, specifier, kind: pattern.kind, line, target: specifier, classification: 'node-builtin' });
        continue;
      }
      try {
        target = pattern.kind === 'require' ? requireFromPrivate.resolve(specifier) : globalThis.Bun.resolveSync(specifier, dirname(privateSourcePath));
      } catch (error) {
        throw new Error(`Bound static ${pattern.kind} edge does not resolve privately: ${sourcePath}:${line} ${specifier}`, { cause: error });
      }
      const relativeTarget = relative(privateRoot, target);
      if (relativeTarget.startsWith(`..${sep}`) || relativeTarget === '..' || isAbsolute(relativeTarget))
        throw new Error(`Private resolution escaped its generated root: ${sourcePath}:${line} ${specifier} -> ${target}`);
      if (target.includes(`${sep}node_modules${sep}`) && !closureTargets.has(target))
        throw new Error(`Resolved package target is outside the selected closure: ${sourcePath}:${line} ${specifier} -> ${target}`);
      edges.push({ from: sourcePath, specifier, kind: pattern.kind, line, target, resolutionMode: pattern.kind === 'require' ? 'require' : 'import', classification: 'private-static-resolution' });
    }
  }
  const dynamicSites = scanDynamicSites(sourcePath, source, mask);
  return { edges, dynamicSites };
}

function countLines(source) {
  return source.length ? source.split('\n').length - (source.endsWith('\n') ? 1 : 0) : 0;
}

function writeOwned(route, target, content, name) {
  return route.writeRetainedFile(target, Buffer.isBuffer(content) ? content : Buffer.from(content), `c2:${name}`);
}

function privateTarget(sourcePath, runtimeRoot, content, attributes = {}) {
  const filename = destinationForSource(sourcePath, runtimeRoot);
  return {
    specifier: filename,
    resolvedPath: relative(runtimeRoot, filename).split(sep).join('/'),
    sha256: sha256(Buffer.isBuffer(content) ? content : Buffer.from(content)),
    ...attributes
  };
}

function dynamicSitePolicy(site, targets) {
  const id = site.siteId;
  if (id === 'DS01') targets.set(id, [privateTarget('frontend/node_modules/playwright/lib/transform/babelBundle.js', site.runtimeRoot, site.generatedSources['frontend/node_modules/playwright/lib/transform/babelBundle.js'])]);
  else if (id === 'DS02') targets.set(id, site.projectInputs.map(input => ({ ...input, projectInput: true })));
  else if (id === 'DS05') targets.set(id, [privateTarget('frontend/node_modules/playwright/package.json', site.runtimeRoot, site.generatedSources['frontend/node_modules/playwright/package.json'])]);
  else if (id >= 'DS06' && id <= 'DS17') targets.set(id, []);
  else if (id === 'DS23' || id === 'DS34') targets.set(id, ['node:module', 'node:worker_threads', 'node:fs', 'node:path', 'node:url'].map(specifier => ({ specifier, builtin: true })));
  else if (id === 'DS24') targets.set(id, [privateTarget('frontend/node_modules/playwright/lib/transform/babelBundle.js', site.runtimeRoot, site.generatedSources['frontend/node_modules/playwright/lib/transform/babelBundle.js'])]);
  else if (id === 'DS25') {
    const inputs = site.projectInputs.map(input => ({ ...input, projectInput: true }));
    const packageFiles = ['frontend/node_modules/@playwright/test/index.js', 'frontend/node_modules/@playwright/test/index.mjs', 'frontend/node_modules/@playwright/test/cli.js'];
    const builtins = ['node:module', 'node:worker_threads', 'node:fs', 'node:path', 'node:url'].map(specifier => ({ specifier, builtin: true }));
    targets.set(id, [...inputs, ...packageFiles.map(path => privateTarget(path, site.runtimeRoot, site.generatedSources[path])), ...builtins]);
  } else if (id === 'DS26' || id === 'DS31' || id === 'DS32') {
    const path = 'frontend/node_modules/playwright-core/package.json';
    targets.set(id, [privateTarget(path, site.runtimeRoot, site.generatedSources[path])]);
  } else if (id === 'DS35' || id === 'DS36' || id === 'DS37') {
    const match = /^`([^`]+)`$/.exec(site.expression);
    if (!match) throw new Error(`C2 browser asset site is not a fixed sibling import: ${id}`);
    const sourcePath = site.from;
    const targetPath = relative(repositoryRoot, resolve(dirname(sourcePath), match[1])).split(sep).join('/');
    const expected = site.generatedSources[targetPath];
    if (typeof expected !== 'string' && !Buffer.isBuffer(expected)) throw new Error(`C2 browser asset target is absent from the private closure: ${id}`);
    const filename = destinationForSource(targetPath, site.runtimeRoot);
    targets.set(id, [{ specifier: filename, assetSpecifier: match[1], resolvedPath: relative(site.runtimeRoot, filename).split(sep).join('/'), sha256: sha256(Buffer.isBuffer(expected) ? expected : Buffer.from(expected)) }]);
  } else targets.set(id, []);
  const approvedTargets = targets.get(id);
  if (approvedTargets.length) return { disposition: 'bound', approvedTargets, enforcedBy: [] };
  if (id >= 'DS06' && id <= 'DS17') return { disposition: 'bound', approvedTargets, enforcedBy: ['DS23', 'DS25'] };
  return { disposition: 'refused', approvedTargets, enforcedBy: [], refusal: { errorName: 'C2DynamicModuleRefusalError' } };
}

function buildDynamicSites(options) {
  const inventory = options.dynamicInventory;
  if (!inventory || inventory.schema !== 1 || inventory.total !== 37 || !Array.isArray(inventory.sites) || inventory.sites.length !== 37)
    throw new Error('C2 dynamic-site inventory is missing or incomplete');
  const generatedSources = options.generatedSources;
  const runtimeRoot = options.runtimeRoot;
  const projectInputs = options.projectInputs;
  const records = [];
  const policyContext = { runtimeRoot, generatedSources, projectInputs };
  const expectedSiteIds = Array.from({ length: 37 }, (_, index) => `DS${String(index + 1).padStart(2, '0')}`);
  if (JSON.stringify(inventory.sites.map(site => site.siteId)) !== JSON.stringify(expectedSiteIds))
    throw new Error('C2 dynamic-site inventory IDs or order changed');
  for (const site of inventory.sites) {
    const originalSource = options.originalSources?.[site.from];
    if (typeof originalSource !== 'string' || sha256(Buffer.from(originalSource)) !== site.originalFileSHA256)
      throw new Error(`C2 dynamic-site original source hash changed: ${site.siteId}`);
    const policy = dynamicSitePolicy({ ...site, ...policyContext }, new Map());
    const generatedFile = relative(runtimeRoot, destinationForSource(site.from, runtimeRoot)).split(sep).join('/');
    const generatedSource = generatedSources[site.from];
    const lines = String(generatedSource).split('\n');
    const guardPattern = new RegExp(`(?:privateC2\\.(?:dynamicModule|enforceDynamicSite)|__c2DynamicAsset)\\("${site.siteId}"`);
    const guardLine = lines.findIndex(line => guardPattern.test(line)) + 1;
    if (!guardLine) throw new Error(`Generated dynamic guard is missing: ${site.siteId}`);
    const generatedSHA256 = sha256(Buffer.isBuffer(generatedSource) ? generatedSource : Buffer.from(generatedSource));
    const originalCoordinate = options.originalCoordinates?.get(site.siteId);
    if (!originalCoordinate || originalCoordinate.path !== site.from)
      throw new Error(`Original dynamic-site coordinate is missing or mismatched: ${site.siteId}`);
    const generatedExpansions = justifiedDynamicSiteExpansions.filter(expansion => expansion.siteId === site.siteId).map(expansion => {
      if (expansion.file !== site.from) throw new Error(`Generated dynamic expansion belongs to another source: ${expansion.expansionId}`);
      const originalIndex = originalSource.indexOf(expansion.originalText);
      if (originalIndex < 0 || originalSource.indexOf(expansion.originalText, originalIndex + 1) >= 0)
        throw new Error(`Authorized dynamic expansion source occurrence is missing or duplicated: ${expansion.expansionId}`);
      const generatedIndex = String(generatedSource).indexOf(expansion.generatedText);
      if (generatedIndex < 0 || String(generatedSource).indexOf(expansion.generatedText, generatedIndex + 1) >= 0)
        throw new Error(`Authorized dynamic expansion output occurrence is missing or duplicated: ${expansion.expansionId}`);
      return {
        expansionId: expansion.expansionId,
        kind: expansion.kind,
        generatedExpressionSHA256: sha256(Buffer.from(expansion.expression)),
        originalCoordinate: { path: expansion.file, line: originalSource.slice(0, originalIndex).split('\n').length, sourceOffset: originalIndex, byteOffset: Buffer.byteLength(originalSource.slice(0, originalIndex)), sourceTextSHA256: sha256(Buffer.from(expansion.originalText)), representation: 'raw-original-source-expansion' },
        generatedCoordinate: { file: generatedFile, line: String(generatedSource).slice(0, generatedIndex).split('\n').length, representation: 'generated-private-source-expansion' },
        justification: expansion.justification
      };
    });
    records.push({
      siteId: site.siteId,
      from: site.from,
      line: site.line,
      expressionSHA256: site.expressionSHA256,
      originalFileSHA256: site.originalFileSHA256,
      inventoryCoordinate: { path: site.from, line: site.line, representation: 'frozen-transformed-source' },
      originalCoordinate,
      generatedCoordinate: { file: generatedFile, line: guardLine, representation: 'generated-private-source' },
      disposition: policy.disposition,
      generatedFile,
      generatedSHA256,
      guardLine,
      generatedExpansions,
      approvedTargets: policy.approvedTargets,
      enforcedBy: policy.enforcedBy,
      ...(policy.refusal ? { refusal: policy.refusal } : {})
    });
  }
  return records;
}

export function generateC2(options) {
  const baselineBytes = readFileSync(options.baselinePath);
  const baselineDigest = sha256(baselineBytes);
  if (baselineDigest !== expectedBaselineSHA256)
    throw new Error(`C2 baseline SHA-256 mismatch: ${baselineDigest}`);
  const baseline = JSON.parse(baselineBytes.toString('utf8'));
  const c1 = c1Input(baseline);
  const { original, closureOriginal, inputReceipts } = readInputs(baseline, c1);
  const declaredVersion = JSON.parse(original['frontend/package.json']).devDependencies?.['@playwright/test'];
  const installedVersion = JSON.parse(text(closureOriginal['frontend/node_modules/@playwright/test/package.json'], '@playwright/test/package.json')).version;
  const playwrightVersion = JSON.parse(text(closureOriginal['frontend/node_modules/playwright/package.json'], 'playwright/package.json')).version;
  if (declaredVersion !== '1.62.1' || installedVersion !== declaredVersion || playwrightVersion !== declaredVersion)
    throw new Error('Selected @playwright/test and transitive Playwright versions differ from the bound C2 package set');
  const selected = selectedOriginals(closureOriginal, original, c1, options.dynamicInventory?.sites);
  const privateRoot = join(options.runtimeRoot, 'project/frontend');
  options.descriptor.runtimeRoot = options.runtimeRoot;
  const originalDynamicSiteScan = options.dynamicInventory
    ? assertOriginalDynamicSiteScanClosed(Object.entries(closureOriginal).filter(([sourcePath]) => /\.(?:[cm]?js|ts)$/.test(sourcePath) && !sourcePath.endsWith('.d.ts')).flatMap(([sourcePath, body]) => scanDynamicSites(sourcePath, body.toString('utf8'))), options.dynamicInventory.sites, closureOriginal)
    : null;
  const originalCoordinates = new Map((originalDynamicSiteScan?.matches || []).map(match => [match.siteId, match.originalCoordinate]));
  const dynamicInputPaths = projectInputs.filter(path => /\.(?:[cm]?js|ts)$/.test(path));
  const dynamicEditSites = (options.dynamicInventory?.sites || []).map(site => {
    const coordinate = originalDynamicSiteScan?.matches.find(match => match.siteId === site.siteId);
    if (!coordinate) throw new Error(`Original dynamic-site occurrence mapping is missing: ${site.siteId}`);
    return { ...site, originalOccurrence: coordinate.originalCoordinate.occurrence, originalCoordinate: coordinate.originalCoordinate, runtimeRoot: options.runtimeRoot, generatedSources: { ...selected.sources, ...closureOriginal, ...original }, originalSource: selected.sources[site.from] };
  });
  const { generated: editedSelected, edits } = applyC2Edits({ ...selected, dynamicSites: dynamicEditSites });
  const dynamicProjectInputs = dynamicInputPaths.map(path => {
    const generated = editedSelected[path] ?? original[path];
    if (typeof generated !== 'string') throw new Error(`C2 dynamic project input is missing: ${path}`);
    const filename = destinationForSource(path, options.runtimeRoot);
    return { specifier: filename, resolvedPath: relative(options.runtimeRoot, filename).split(sep).join('/'), path: relative(options.runtimeRoot, filename).split(sep).join('/'), sha256: sha256(Buffer.from(generated)), projectInput: true };
  });
  options.descriptor.dynamicProjectInputs = dynamicProjectInputs.map(({ path, sha256 }) => ({ path, sha256 }));
  const vendorOutputs = new Map();
  for (const [path, body] of Object.entries(original)) vendorOutputs.set(path, Buffer.from(body));
  for (const [path, body] of Object.entries(closureOriginal)) vendorOutputs.set(path, body);
  for (const [path, body] of Object.entries(editedSelected)) vendorOutputs.set(path, body);
  const supportOutputs = {
    'privateCompletion.cjs': c1.support,
    'c2-bootstrap.cjs': text(readFileSync(join(scriptDirectory, 'c2-bootstrap.cjs')), 'c2-bootstrap.cjs'),
    'c2-paths.cjs': text(readFileSync(join(scriptDirectory, 'c2-paths.cjs')), 'c2-paths.cjs'),
    'c2-route.cjs': text(readFileSync(join(scriptDirectory, 'c2-route.cjs')), 'c2-route.cjs')
  };
  const privateFiles = [];
  for (const [sourcePath, body] of vendorOutputs) {
    const target = destinationForSource(sourcePath, options.runtimeRoot);
    const patchedSource = editedSelected[sourcePath];
    writeOwned(options.route, target, patchedSource ?? body, `module:${sourcePath}`);
    const originalBody = closureOriginal[sourcePath] || Buffer.from(original[sourcePath]);
    const originalText = originalBody.toString('utf8');
    const generatedText = Buffer.isBuffer(patchedSource ?? body) ? (patchedSource ?? body).toString('utf8') : patchedSource ?? body;
    privateFiles.push({ source: sourcePath, privatePath: target, originalSHA256: sha256(originalBody), generatedSHA256: sha256(Buffer.isBuffer(patchedSource ?? body) ? patchedSource ?? body : Buffer.from(patchedSource ?? body)), originalBytes: originalBody.length, generatedBytes: Buffer.byteLength(patchedSource ?? body), originalLines: countLines(originalText), generatedLines: countLines(generatedText), c1InputSHA256: sourcePath === 'frontend/node_modules/playwright/lib/runner/index.js' ? c1.runnerIdentity.sha256 : undefined, patched: patchedSource !== undefined });
  }
  const supportDir = join(privateRoot, 'private');
  for (const [name, body] of Object.entries(supportOutputs)) {
    const target = join(supportDir, name);
    writeOwned(options.route, target, body, `support:${name}`);
    privateFiles.push({ source: `frontend/scripts/private-qualification/${name}`, privatePath: target, originalSHA256: sha256(Buffer.from(body)), generatedSHA256: sha256(Buffer.from(body)), originalBytes: Buffer.byteLength(body), generatedBytes: Buffer.byteLength(body), originalLines: countLines(body), generatedLines: countLines(body), patched: false });
  }
  const releaseFixturePath = join(privateRoot, 'tests/browser/release-resources.spec.ts');
  const generatedReleaseFixture = editedSelected['frontend/tests/browser/release-resources.spec.ts'];
  if (!generatedReleaseFixture) throw new Error('C2 private release fixture was not generated');
  const releaseSource = original['frontend/tests/browser/release-resources.spec.ts'];
  const generatedSources = { ...original };
  for (const [sourcePath, body] of Object.entries(closureOriginal)) {
    if (editedSelected[sourcePath] !== undefined) generatedSources[sourcePath] = editedSelected[sourcePath];
    else if (/\.(?:[cm]?js|ts|json)$/.test(sourcePath)) generatedSources[sourcePath] = text(body, sourcePath);
  }
  Object.assign(generatedSources, selected.sources, editedSelected);
  const dynamicSiteRecords = options.dynamicInventory ? buildDynamicSites({ dynamicInventory: options.dynamicInventory, generatedSources, originalSources: selected.sources, originalCoordinates, runtimeRoot: options.runtimeRoot, projectInputs: dynamicProjectInputs }) : [];
  const originalDynamicSiteSemantics = options.dynamicInventory
    ? assertOriginalDynamicSiteSemanticsUnambiguous(originalDynamicSiteScan, dynamicSiteRecords)
    : null;
  let dynamicManifestPath;
  let dynamicManifestSHA256;
  if (options.dynamicInventory) {
    const dynamicManifest = { schema: 1, invocationId: options.descriptor.invocationId, root: options.descriptor.root, runtimeRoot: options.runtimeRoot, inventorySHA256: options.dynamicInventorySHA256, sites: dynamicSiteRecords.map(({ siteId, disposition, approvedTargets, enforcedBy, refusal }) => ({ siteId, disposition, approvedTargets, enforcedBy, ...(refusal ? { refusal } : {}) })) };
    const dynamicManifestBytes = Buffer.from(JSON.stringify(dynamicManifest, null, 2) + '\n');
    dynamicManifestSHA256 = sha256(dynamicManifestBytes);
    dynamicManifestPath = join(privateRoot, 'private/dynamic-module-manifest.json');
    options.descriptor.dynamicManifestPath = dynamicManifestPath;
    options.descriptor.dynamicManifestSHA256 = dynamicManifestSHA256;
    writeOwned(options.route, dynamicManifestPath, dynamicManifestBytes, 'dynamic-module-manifest');
    privateFiles.push({ source: 'frontend/scripts/private-qualification/dynamic-module-manifest.json', privatePath: dynamicManifestPath, originalSHA256: null, generatedSHA256: dynamicManifestSHA256, originalBytes: 0, generatedBytes: dynamicManifestBytes.length, originalLines: 0, generatedLines: countLines(dynamicManifestBytes.toString('utf8')), patched: true });
  }
  const edges = [];
  const scannedDynamicSites = [];
  const closureTargets = new Set(privateFiles.map(item => item.privatePath));
  for (const source of privateFiles) {
    if (!/\.(?:[cm]?js|ts)$/.test(source.privatePath) || source.privatePath.endsWith('.d.ts')) continue;
    const body = vendorOutputs.get(source.source);
    const scanned = moduleEdges(source.source, Buffer.isBuffer(body) ? body.toString('utf8') : body ?? readFileSync(source.privatePath, 'utf8'), source.privatePath, privateRoot, closureTargets);
    edges.push(...scanned.edges);
    scannedDynamicSites.push(...scanned.dynamicSites);
  }
  const generatedDynamicSiteScan = options.dynamicInventory
    ? assertGeneratedDynamicScanClosed(scannedDynamicSites, dynamicSiteRecords, generatedSources)
    : null;
  const patchText = edits.map(edit => {
    const input = selected.sources[edit.file] || original[edit.file];
    const generatedSource = editedSelected[edit.file] || input;
    const { oldText, newText, ...metadata } = edit;
    return {
      ...metadata,
      ...(typeof oldText === 'string' ? { oldBytes: Buffer.byteLength(oldText), oldSHA256: sha256(Buffer.from(oldText)) } : {}),
      ...(typeof newText === 'string' ? { newBytes: Buffer.byteLength(newText), newSHA256: sha256(Buffer.from(newText)) } : {}),
      ...(input ? { inputSHA256: sha256(Buffer.from(input)) } : {}),
      ...(generatedSource ? { generatedSHA256: sha256(Buffer.from(generatedSource)) } : {})
    };
  });
  const c1History = {
    record: '3ea2ca12-526b-46d1-8efa-31dfdbccf3d4',
    runnerSHA256: c1.runnerIdentity.sha256,
    supportSHA256: c1.supportIdentity.sha256,
    completionSHA256: c1.completionIdentity.sha256,
    summarySHA256: c1.summaryIdentity.sha256,
    cases: 79,
    passed: 79,
    variants: 80,
    reusedUnchanged: true
  };
  const sourceManifest = {
    schema: 1,
    phase: 'C2',
    invocationId: options.descriptor.invocationId,
    inputBaseline: { path: options.baselinePath, sha256: baselineDigest },
    c1: c1History,
    selectedEntry: { package: '@playwright/test', version: '1.62.1', source: 'frontend/node_modules/@playwright/test/cli.js', generatedPath: join(privateRoot, 'node_modules/@playwright/test/cli.js'), bootstrapBeforeRequireAndParse: true },
    closure: {
      baselineEntries: baseline.closureCandidate.length,
      copiedEntries: privateFiles.filter(item => item.source.startsWith('frontend/node_modules/')).length,
      excludedOptional: baseline.closureCandidate.filter(item => item.path.includes('/node_modules/playwright/node_modules/fsevents/')).map(item => ({ path: item.path, reason: 'watch mode is refused before program effects' })),
      candidateLogicalBytes: baseline.closureTotals.logicalBytes,
      staticEdges: edges,
      dynamicSites: options.dynamicInventory ? dynamicSiteRecords : scannedDynamicSites,
      dynamicSiteScan: options.dynamicInventory ? { original: originalDynamicSiteScan, originalSemantics: originalDynamicSiteSemantics, generated: generatedDynamicSiteScan } : null,
      dynamicModuleManifest: dynamicManifestPath ? { path: dynamicManifestPath, sha256: dynamicManifestSHA256, sites: dynamicSiteRecords.length } : null,
      resolutionClassification: options.dynamicInventory ? 'Every inventoried dynamic site is bound or refused by generated guards and the invocation-bound private manifest; the generated scan is closed against the inventory.' : 'Bound static private resolutions and enumerated dynamic sites; the generated code is never loaded as a full bundle during C2.'
    },
    projectInputs: inputReceipts,
    generatedFiles: privateFiles,
    vendorEdits: patchText.filter(edit => edit.file.startsWith('frontend/node_modules/')),
    fixtureEdits: patchText.filter(edit => edit.file === 'frontend/tests/browser/release-resources.spec.ts'),
    vendorCounts: patchText.filter(edit => edit.file.startsWith('frontend/node_modules/')).reduce((sum, edit) => ({
      added: sum.added + Math.max(0, countLines(edit.newText || '') - countLines(edit.oldText || '')),
      deleted: sum.deleted + Math.max(0, countLines(edit.oldText || '') - countLines(edit.newText || ''))
    }), { added: 0, deleted: 0 }),
    supportCounts: Object.fromEntries(Object.entries(supportOutputs).map(([name, body]) => [name, { lines: countLines(body), bytes: Buffer.byteLength(body) }])),
    dynamicModuleManifest: dynamicManifestPath ? { path: dynamicManifestPath, sha256: dynamicManifestSHA256, lines: countLines(readFileSync(dynamicManifestPath, 'utf8')) } : null,
    fixture: { sourcePath: 'frontend/tests/browser/release-resources.spec.ts', sourceSHA256: sha256(Buffer.from(releaseSource)), sourceLines: countLines(releaseSource), generatedPath: releaseFixturePath, generatedSHA256: sha256(Buffer.from(generatedReleaseFixture)), generatedLines: countLines(generatedReleaseFixture), assertionsPreserved: true },
    patchReconstruction: { edits: patchText, sha256: sha256(Buffer.from(JSON.stringify(patchText))) },
    output: { root: options.runtimeRoot, files: privateFiles.length, logicalBytes: privateFiles.reduce((sum, item) => sum + item.generatedBytes, 0) }
  };
  const manifestPath = join(options.invocationRoot, 'source-manifest.json');
  writeOwned(options.route, manifestPath, JSON.stringify(sourceManifest, null, 2) + '\n', 'source-manifest');
  return { manifest: sourceManifest, generated: editedSelected, original: selected.sources, projectSources: original, releaseFixture: generatedReleaseFixture, manifestPath, dynamicSites: dynamicSiteRecords, dynamicEditSites, dynamicManifestPath, dynamicManifestSHA256 };
}
