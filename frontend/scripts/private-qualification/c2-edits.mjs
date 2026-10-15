import { createHash } from 'node:crypto';
import { dirname as dirnamePosix, relative as relativePosix } from 'node:path';

function replaceUnique(source, oldText, newText, label, file, records) {
  const first = source.indexOf(oldText);
  if (first < 0 || source.indexOf(oldText, first + oldText.length) >= 0)
    throw new Error(`C2 source edit is not unique: ${file}:${label}`);
  const result = source.slice(0, first) + newText + source.slice(first + oldText.length);
  records.push({ file, label, start: first, oldBytes: Buffer.byteLength(oldText), newBytes: Buffer.byteLength(newText), oldSHA256: hash(oldText), newSHA256: hash(newText), oldText, newText });
  return result;
}

function hash(value) {
  return createHash('sha256').update(value).digest('hex');
}

export const justifiedDynamicSiteExpansions = [
  {
    siteId: 'DS02',
    expansionId: 'DS02-ESM-import',
    file: 'frontend/node_modules/playwright/lib/common/index.js',
    kind: 'import',
    originalText: 'const esmImport = () => eval(`import(${JSON.stringify(fileName)})`);',
    generatedText: 'const esmImport = () => import(privateC2.dynamicModule("DS02", "import", () => fileName.href, __filename));',
    expression: 'fileName.href',
    justification: 'The frozen syntax scan excludes the import embedded in this eval template, so the exact source occurrence receives an explicit private import guard.'
  },
  {
    siteId: 'DS02',
    expansionId: 'DS02-ESM-preflight',
    file: 'frontend/node_modules/playwright/lib/common/index.js',
    kind: 'import',
    originalText: 'await eval(`import(${JSON.stringify(fileName + ".esm.preflight")})`).catch((error) => debugTest("Failed to load preflight for " + file + ", source maps may be missing for errors thrown during loading.", error)).finally(nextTask);',
    generatedText: 'await Promise.resolve().then(() => import(privateC2.dynamicModule("DS02", "import", () => fileName.href + ".esm.preflight", __filename))).catch((error) => debugTest("Failed to load preflight for " + file + ", source maps may be missing for errors thrown during loading.", error)).finally(nextTask);',
    expression: 'fileName.href + ".esm.preflight"',
    justification: 'The frozen syntax scan excludes the preflight import embedded in this eval template, so the exact source occurrence receives an explicit private import guard.'
  }
];

function sourceMask(source) {
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
      const end = source.indexOf('*/', index + 2);
      index = end < 0 ? source.length : end + 2;
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

export function legacyDynamicExpression(expression) {
  const end = expression.indexOf(')');
  return expression.slice(0, end < 0 ? 160 : Math.min(end, 160)).trim();
}

export function scanDynamicCalls(source) {
  const mask = sourceMask(source);
  const regex = /\b(require|import)\s*\(/g;
  const matches = [];
  for (const match of source.matchAll(regex)) {
    if (!mask[match.index]) continue;
    const kind = match[1];
    const open = source.indexOf('(', match.index);
    let depth = 0;
    let quote = '';
    let escaped = false;
    let close = -1;
    for (let index = open; index < source.length; index += 1) {
      if (!mask[index]) continue;
      const character = source[index];
      if (quote) {
        if (escaped) escaped = false;
        else if (character === '\\') escaped = true;
        else if (character === quote) quote = '';
        continue;
      }
      if (character === "'" || character === '"' || character === '`') {
        quote = character;
        continue;
      }
      if (character === '/' && source[index + 1] === '/') {
        while (index < source.length && source[index] !== '\n') index += 1;
        continue;
      }
      if (character === '/' && source[index + 1] === '*') {
        const end = source.indexOf('*/', index + 2);
        index = end < 0 ? source.length : end + 1;
        continue;
      }
      if (character === '(') depth += 1;
      else if (character === ')' && --depth === 0) {
        close = index;
        break;
      }
    }
    if (close < 0) throw new Error(`Dynamic module call is unterminated at offset ${match.index}`);
    const expressionStart = open + 1;
    const expressionEnd = close;
    const expression = source.slice(expressionStart, expressionEnd).trim();
    const noArgumentMethod = !expression && (source[match.index - 1] === '.' || /^\s*\{/.test(source.slice(close + 1)));
    if ((!expression && !noArgumentMethod) || /^["']/.test(expression)) continue;
    const leading = source.slice(expressionStart, expressionEnd).match(/^\s*/)?.[0].length ?? 0;
    const legacyExpression = noArgumentMethod ? /\s*([\s\S]+?)\)/.exec(source.slice(open + 1))?.[1].trim() ?? '' : legacyDynamicExpression(expression);
    const call = {
      kind,
      start: match.index,
      open,
      close,
      expressionStart: expressionStart + leading,
      expressionEnd,
      expression,
      legacyExpression,
      line: source.slice(0, match.index).split('\n').length
    };
    matches.push(call);
  }
  return matches;
}

function dynamicCallMatches(source) {
  return scanDynamicCalls(source);
}

function dynamicExpressionMatches(source) {
  return scanDynamicCalls(source).map(match => ({
    expression: match.legacyExpression,
    line: match.line,
    index: match.start
  }));
}

function bootstrapRequire(sourcePath) {
  const relative = relativePosix(dirnamePosix(sourcePath), 'frontend/private/c2-bootstrap.cjs');
  return relative.startsWith('.') ? relative : `./${relative}`;
}

function installNodeBootstrap(sourcePath, source) {
  if (source.includes('var privateC2 = require(')) return source;
  const statement = `var privateC2 = require(${JSON.stringify(bootstrapRequire(sourcePath))});\n`;
  const strict = source.match(/^([\s\uFEFF]*["']use strict["'];[^\n]*\n)/);
  return strict ? source.slice(0, strict[0].length) + statement + source.slice(strict[0].length) : statement + source;
}

function siteMarker(source, site, call) {
  const guard = `privateC2.enforceDynamicSite(${JSON.stringify(site.siteId)}, ["DS23", "DS25"]);`;
  const afterCall = source.slice(call.close + 1);
  const body = /^\s*\{/.exec(afterCall);
  if (body) {
    const brace = call.close + 1 + body[0].lastIndexOf('{');
    const lineStart = source.lastIndexOf('\n', call.start - 1) + 1;
    const indent = source.slice(lineStart, call.start).match(/^\s*/)?.[0] ?? '';
    return { index: brace + 1, text: `\n${indent}  ${guard}` };
  }
  const lineStart = source.lastIndexOf('\n', call.start - 1) + 1;
  const indent = source.slice(lineStart, call.start).match(/^\s*/)?.[0] ?? '';
  return { index: lineStart, text: `${indent}${guard}\n` };
}

function applyDynamicSiteEdits(generated, originalSources, sites) {
  if (!sites?.length) return [];
  const edits = [];
  const byFile = new Map();
  for (const site of sites) {
    const source = generated[site.from];
    if (typeof source !== 'string') throw new Error(`C2 dynamic site source is missing: ${site.siteId}`);
    const matches = dynamicExpressionMatches(source).filter(match => hash(match.expression) === site.expressionSHA256);
    const originalSource = originalSources[site.from];
    if (typeof originalSource !== 'string') throw new Error(`C2 dynamic original source is missing: ${site.siteId}`);
    const originalSourceMatches = dynamicExpressionMatches(originalSource).filter(match => hash(match.expression) === site.expressionSHA256);
    const sameExpressionSites = sites.filter(candidate => candidate.from === site.from && candidate.expressionSHA256 === site.expressionSHA256);
    const occurrenceIndexes = sameExpressionSites.map(candidate => candidate.originalOccurrence).sort((left, right) => left - right);
    const ordinal = site.originalOccurrence;
    if (!Number.isSafeInteger(ordinal) || ordinal < 0 || originalSourceMatches.length !== sameExpressionSites.length || matches.length !== sameExpressionSites.length || occurrenceIndexes.some((value, index) => value !== index) || !matches[ordinal])
      throw new Error(`C2 dynamic site occurrence mapping drifted: ${site.siteId}`);
    const calls = dynamicCallMatches(source);
    const call = calls.find(match => match.start === matches[ordinal].index);
    if (!call) throw new Error(`C2 dynamic call boundary is missing: ${site.siteId}`);
    const fileEdits = byFile.get(site.from) || [];
    fileEdits.push({ site, call });
    byFile.set(site.from, fileEdits);
  }
  for (const [file, fileEdits] of byFile) {
    const oldSource = generated[file];
    let source = oldSource;
    const delegating = fileEdits.filter(({ site }) => site.siteId >= 'DS06' && site.siteId <= 'DS17');
    const browserAssets = fileEdits.filter(({ site }) => ['DS35', 'DS36', 'DS37'].includes(site.siteId));
    const delegatedIds = new Set(delegating.map(({ site }) => site.siteId));
    const browserIds = new Set(browserAssets.map(({ site }) => site.siteId));
    const nodeCalls = fileEdits.filter(({ site }) => !delegatedIds.has(site.siteId) && !browserIds.has(site.siteId));
    const insertions = [];
    for (const { site, call } of delegating) insertions.push(siteMarker(source, site, call));
    for (const { site, call } of nodeCalls) {
      const expression = call.expression;
      const kind = call.kind;
      const guarded = `privateC2.dynamicModule(${JSON.stringify(site.siteId)}, ${JSON.stringify(kind)}, () => (${expression}), __filename)`;
      insertions.push({ index: call.open + 1, end: call.close, text: guarded });
    }
    for (const { site, call } of browserAssets) {
      const guarded = `__c2DynamicAsset(${JSON.stringify(site.siteId)}, ${call.expression})`;
      insertions.push({ index: call.open + 1, end: call.close, text: guarded });
    }
    insertions.sort((left, right) => right.index - left.index);
    for (const edit of insertions) {
      if (edit.end === undefined) source = source.slice(0, edit.index) + edit.text + source.slice(edit.index);
      else source = source.slice(0, edit.index) + edit.text + source.slice(edit.end + (edit.index === edit.start ? 0 : 0));
    }
    if (browserAssets.length) {
      const targets = Object.fromEntries(browserAssets.map(({ site, call }) => [site.siteId, call.expression.slice(1, -1)]));
      const helper = `const __c2DynamicAssetTargets = ${JSON.stringify(targets)};\nfunction __c2DynamicAsset(siteId, specifier) {\n  if (__c2DynamicAssetTargets[siteId] !== specifier) { const error = new Error("C2 dynamic module refused at " + siteId); error.name = "C2DynamicModuleRefusalError"; error.siteId = siteId; throw error; }\n  return specifier;\n}\n`;
      source = helper + source;
    } else if (nodeCalls.length || delegating.length) {
      source = installNodeBootstrap(file, source);
    }
    generated[file] = source;
    edits.push({ file, label: 'dynamic module guards', siteIds: fileEdits.map(({ site }) => site.siteId), oldText: oldSource, newText: source, generatedSHA256: hash(source) });
  }
  return edits;
}

export function applyC2Edits(input) {
  const edits = [];
  const generated = { ...input.sources };
  const shimName = 'frontend/node_modules/@playwright/test/cli.js';
  generated[shimName] = replaceUnique(generated[shimName],
    "const { program } = require('playwright/lib/program');",
    "const privateC2 = require('../../../private/c2-bootstrap.cjs');\nprivateC2.beforeProgramLoad();\nconst { program } = require('playwright/lib/program');",
    'selected shim configures private bootstrap before program require and parse', shimName, edits);

  const commonName = 'frontend/node_modules/playwright/lib/common/index.js';
  generated[commonName] = replaceUnique(generated[commonName],
    'var index_exports = {};',
    'var privateC2 = require("../../../../private/c2-bootstrap.cjs");\nvar index_exports = {};',
    'common module uses canonical invocation bootstrap', commonName, edits);
  generated[commonName] = replaceUnique(generated[commonName],
    'var FullConfigInternal = class {\n  constructor(location, userConfig, configCLIOverrides, metadata) {\n    this.projects = [];\n    this.defineConfigWasUsed = false;\n    this.globalSetups = [];\n    this.globalTeardowns = [];\n    if (configCLIOverrides.projects && userConfig.projects)\n      throw new Error(`Cannot use --browser option when configuration file defines projects. Specify browserName in the projects instead.`);\n    const { resolvedConfigFile, configDir } = location;\n    const packageJsonPath = getPackageJsonPath(configDir);\n    const packageJsonDir = packageJsonPath ? import_path3.default.dirname(packageJsonPath) : process.cwd();',
    'var FullConfigInternal = class {\n  constructor(location, userConfig, configCLIOverrides, metadata) {\n    this.projects = [];\n    this.defineConfigWasUsed = false;\n    this.globalSetups = [];\n    this.globalTeardowns = [];\n    if (configCLIOverrides.projects && userConfig.projects)\n      throw new Error(`Cannot use --browser option when configuration file defines projects. Specify browserName in the projects instead.`);\n    const { resolvedConfigFile, configDir } = location;\n    const packageJsonPath = getPackageJsonPath(configDir);\n    const packageJsonDir = packageJsonPath ? import_path3.default.dirname(packageJsonPath) : process.cwd();\n    privateC2.getRuntime().route.validateConfig({ location, userConfig, configCLIOverrides, packageJsonDir, argv: configCLIOverrides.argv, env: process.env });',
    'all original project options validate before reporter or plugin setup', commonName, edits);
  generated[commonName] = replaceUnique(generated[commonName],
    '    this._assignUniqueProjectIds(this.projects);\n    this.config.projects = this.projects.map((p) => p.project);',
    '    this._assignUniqueProjectIds(this.projects);\n    privateC2.getRuntime().route.routeProjects(this.projects, this.configCLIOverrides);\n    this.config.projects = this.projects.map((p) => p.project);',
    'validated all-project route maps unique project IDs before downstream use', commonName, edits);
  generated[commonName] = replaceUnique(generated[commonName],
    'async function deserializeConfig(data) {\n  if (data.compilationCache)',
    'async function deserializeConfig(data) {\n  privateC2.getRuntime().route.validateSerializedRoute(data.configCLIOverrides);\n  if (data.compilationCache)',
    'worker route is validated before compilation cache import', commonName, edits);
  generated[commonName] = replaceUnique(generated[commonName],
    'function serializeConfig(config, passCompilationCache) {\n  const result2 = {',
    'function serializeConfig(config, passCompilationCache) {\n  privateC2.getRuntime().route.validateSerializedRoute(config.configCLIOverrides);\n  const result2 = {',
    'worker serialization carries a validated original/internal route distinction', commonName, edits);
  generated[commonName] = replaceUnique(generated[commonName],
    'var cacheDir = process.env.PWTEST_CACHE_DIR || (() => {\n  if (process.platform === "win32")\n    return import_path.default.join(import_os.default.tmpdir(), `playwright-transform-cache`);\n  return import_path.default.join(import_os.default.tmpdir(), `playwright-transform-cache-` + process.geteuid?.());\n})();',
    'var cacheDir = privateC2.getRuntime().route.transformCacheDir();',
    'transform cache is pinned to the private invocation', commonName, edits);
  const originalCodeCacheWriter = [
    'function writeCodeCache(codePath, code) {',
    '  import_fs.default.writeFileSync(codePath, `// ${calculateSha1(code)}',
    '${code}`, "utf8");',
    '}'
  ].join(String.fromCharCode(10));
  const privateCodeCacheWriter = [
    'function writeCodeCache(codePath, code, actor) {',
    '  privateC2.getRuntime().route.writeCacheFile(codePath, `// ${calculateSha1(code)}',
    '${code}`, actor);',
    '}'
  ].join(String.fromCharCode(10));
  generated[commonName] = replaceUnique(generated[commonName], originalCodeCacheWriter, privateCodeCacheWriter,
    'code cache writer uses exclusive private file admission', commonName, edits);
  generated[commonName] = replaceUnique(generated[commonName],
    '      if ((0, import_globals.isWorkerProcess)())\n        return {};\n      clearOldCacheEntries(cacheFolderName, filePathHash);\n      import_fs.default.mkdirSync(import_path.default.dirname(cachePath), { recursive: true });\n      if (map)\n        import_fs.default.writeFileSync(sourceMapPath, JSON.stringify(map), "utf8");\n      if (data.size)\n        import_fs.default.writeFileSync(dataPath, JSON.stringify(Object.fromEntries(data.entries()), void 0, 2), "utf8");\n      writeCodeCache(codePath, code);',
    '      if ((0, import_globals.isWorkerProcess)())\n        return {};\n      const actor = `transform:${filePathHash}:${contentHash.substring(0, 7)}`;\n      if (!privateC2.getRuntime().route.cacheWritesAllowed())\n        return {};\n      clearOldCacheEntries(cacheFolderName, filePathHash);\n      if (map)\n        privateC2.getRuntime().route.writeCacheFile(sourceMapPath, JSON.stringify(map), actor);\n      if (data.size)\n        privateC2.getRuntime().route.writeCacheFile(dataPath, JSON.stringify(Object.fromEntries(data.entries()), void 0, 2), actor);\n      writeCodeCache(codePath, code, actor);',
    'cache JS map and data writes wait for full option validation and are retained', commonName, edits);
  generated[commonName] = replaceUnique(generated[commonName],
    'function clearOldCacheEntries(cacheFolderName, filePathHash) {\n  const cachePath = import_path.default.join(cacheDir, cacheFolderName);\n  try {\n    const cachedRelevantFiles = import_fs.default.readdirSync(cachePath).filter((file2) => file2.startsWith(filePathHash));\n    for (const file2 of cachedRelevantFiles)\n      import_fs.default.rmSync(import_path.default.join(cachePath, file2), { force: true });\n  } catch {\n  }\n}',
    'function clearOldCacheEntries(cacheFolderName, filePathHash) {\n  const cachePath = import_path.default.join(cacheDir, cacheFolderName);\n  if (!privateC2.getRuntime().route.cacheWritesAllowed())\n    return;\n  privateC2.getRuntime().route.retainCacheDirectory(cachePath, filePathHash);\n}',
    'cache invalidation becomes non-deleting retention verification', commonName, edits);

  const runnerName = 'frontend/node_modules/playwright/lib/runner/index.js';
  generated[runnerName] = replaceUnique(generated[runnerName],
    'var privateCompletion = require("./privateCompletion.cjs");',
    'var privateCompletion = require("../../../../private/privateCompletion.cjs");\nvar privateC2 = require("../../../../private/c2-bootstrap.cjs");',
    'runner consumes the canonical support module', runnerName, edits);
  generated[runnerName] = replaceUnique(generated[runnerName],
    'function createRemoveOutputDirsTask() {\n  return {\n    title: "clear output",\n    setup: async (testRun) => {\n      if (testRun.options.preserveOutputDir)\n        return;\n      const outputDirs = /* @__PURE__ */ new Set();\n      testRun.filteredProjects.forEach((p) => outputDirs.add(p.project.outputDir));\n      await Promise.all(Array.from(outputDirs).map((outputDir) => removeFolders4([outputDir]).then(async ([error]) => {\n        if (!error)\n          return;\n        if (error.code === "EBUSY") {\n          const entries = await readDirAsync2(outputDir).catch((e) => []);\n          await Promise.all(entries.map((entry) => removeFolders4([import_path15.default.join(outputDir, entry)])));\n        } else {\n          throw error;\n        }\n      })));\n    }\n  };\n}',
    'function createRemoveOutputDirsTask() {\n  return {\n    title: "clear output",\n    setup: async (testRun) => {\n      if (testRun.options.preserveOutputDir)\n        return;\n      for (const project of testRun.filteredProjects)\n        privateC2.getRuntime().route.verifyProjectOutput(project.id || project.project.name);\n    }\n  };\n}',
    'clear output verifies admitted project roots and never removes EBUSY children', runnerName, edits);
  generated[runnerName] = replaceUnique(generated[runnerName],
    '    this._listMode = !!listMode;\n    const override = lastFailedFileOverride ?? process.env.PLAYWRIGHT_LAST_RUN_OUTPUT_FILE;\n    if (override) {\n      this._lastRunFile = import_path16.default.resolve(process.cwd(), override);\n    } else {\n      const [project] = filteredProjects;\n      if (project)\n        this._lastRunFile = import_path16.default.join(project.project.outputDir, ".last-run.json");\n    }',
    '    this._listMode = !!listMode;\n    const override = lastFailedFileOverride ?? process.env.PLAYWRIGHT_LAST_RUN_OUTPUT_FILE;\n    if (override)\n      throw new Error("Private LastRun path overrides are not supported");\n    const [project] = filteredProjects;\n    if (project)\n      this._lastRunFile = privateC2.getRuntime().route.lastRunFile(project.id || project.project.name);',
    'LastRun is routed under the admitted first-project result root', runnerName, edits);
  generated[runnerName] = replaceUnique(generated[runnerName],
    '    await import_fs12.default.promises.mkdir(import_path16.default.dirname(this._lastRunFile), { recursive: true });\n    await import_fs12.default.promises.writeFile(this._lastRunFile, JSON.stringify(lastRunInfo, void 0, 2));',
    '    privateC2.getRuntime().route.writeRetainedFile(this._lastRunFile, JSON.stringify(lastRunInfo, void 0, 2), "last-run");',
    'LastRun file uses exclusive retained writer admission', runnerName, edits);
  generated[runnerName] = replaceUnique(generated[runnerName],
    'async function runAllTestsWithConfig(config2, options) {\n  setPlaywrightTestProcessEnv();',
    'async function runAllTestsWithConfig(config2, options) {\n  privateC2.assertExecutionReady();\n  setPlaywrightTestProcessEnv();',
    'runner fails closed before plugin and reporter effects', runnerName, edits);

  const workerName = 'frontend/node_modules/playwright/lib/worker/workerProcessEntry.js';
  generated[workerName] = replaceUnique(generated[workerName],
    '"use strict";',
    '"use strict";\nvar privateC2 = require("../../../../private/c2-bootstrap.cjs");\nprivateC2.beforeWorkerLoad();',
    'worker entry configures its invocation-bound module instance and checks readiness before common imports', workerName, edits);
  generated[workerName] = replaceUnique(generated[workerName],
    '      for (const file of this._temporaryTraceFiles)\n        await import_fs2.default.promises.unlink(file).catch(() => {\n        });',
    '      for (const file of this._temporaryTraceFiles)\n        privateC2.getRuntime().route.retainAdmittedPath(file);',
    'passing trace discard retains raw source files without exporting attachments', workerName, edits);
  generated[workerName] = replaceUnique(generated[workerName],
    '    const file = import_path2.default.join(this._artifactsDir, createGuid() + ".zip");\n    this._temporaryTraceFiles.push(file);',
    '    const file = import_path2.default.join(this._artifactsDir, createGuid() + ".zip");\n    privateC2.getRuntime().route.admitWritableFile(file, `trace:${this._testInfo.testId}:${this._temporaryTraceFiles.length}`);\n    this._temporaryTraceFiles.push(file);',
    'raw trace ZIP path is admitted before the writer opens it', workerName, edits);
  generated[workerName] = replaceUnique(generated[workerName],
    '    await import_fs2.default.promises.rename(temporaryTraceFiles[0], fileName);',
    '    privateC2.getRuntime().route.relocateAdmittedFile(temporaryTraceFiles[0], fileName);',
    'single-source trace relocation records identity and refuses collision', workerName, edits);
  generated[workerName] = replaceUnique(generated[workerName],
    'zipFile.outputStream.pipe(import_fs2.default.createWriteStream(fileName)).on("close", () => {\n      void Promise.all(temporaryTraceFiles.map((tempFile) => import_fs2.default.promises.unlink(tempFile))).then(() => {',
    'privateC2.getRuntime().route.admitWritableFile(fileName, `trace-merge:${temporaryTraceFiles[0]}`);\n      zipFile.outputStream.pipe(import_fs2.default.createWriteStream(fileName, { flags: "r+" })).on("close", () => {\n        void Promise.all(temporaryTraceFiles.map((tempFile) => privateC2.getRuntime().route.retainAdmittedPath(tempFile))).then(() => {',
    'merged trace is admitted before writing and source archives remain retained', workerName, edits);
  generated[workerName] = replaceUnique(generated[workerName],
    '    import_fs3.default.mkdirSync(this.outputDir, { recursive: true });\n    return outputPath;',
    '    privateC2.getRuntime().route.admitTestOutput(this, outputPath);\n    return outputPath;',
    'TestInfo outputPath enrolls its real project and retry path', workerName, edits);
  generated[workerName] = replaceUnique(generated[workerName],
    '      if (this._isStopped || isSkipped) {\n        testInfo.status = "skipped";\n        return;\n      }\n      await removeFolders([testInfo.outputDir]);',
    '      if (this._isStopped || isSkipped) {\n        testInfo.status = "skipped";\n        return;\n      }\n      privateC2.getRuntime().route.admitTestOutput(testInfo);',
    'first test output admission replaces recursive deletion', workerName, edits);
  generated[workerName] = replaceUnique(generated[workerName],
    '    if (!preserveOutput)\n      await removeFolders([testInfo.outputDir]);',
    '    if (!preserveOutput)\n      privateC2.getRuntime().route.retainTestOutput(testInfo);',
    'final test output cleanup retains passing and failed paths', workerName, edits);

  generated[workerName] = replaceUnique(generated[workerName],
    'import_fs2.default.createWriteStream(this._generateNextTraceRecordingPath())',
    'import_fs2.default.createWriteStream(this._generateNextTraceRecordingPath(), { flags: "r+" })',
    'trace zip file opens the exclusively reserved empty output without truncation', workerName, edits);

  const coreName = 'frontend/node_modules/playwright-core/lib/coreBundle.js';
  generated[coreName] = replaceUnique(generated[coreName],
    '"use strict";\nvar __create = Object.create;',
    '"use strict";\nvar privateC2 = require("../../../private/c2-bootstrap.cjs");\nprivateC2.assertExecutionReady();\nvar __create = Object.create;',
    'core native readiness guard precedes browser and process helpers', coreName, edits);
  generated[coreName] = replaceUnique(generated[coreName],
    '    bidiMapper = __toESM(require("chromium-bidi/lib/cjs/bidiMapper/BidiMapper"));\n    bidiCdpConnection = __toESM(require("chromium-bidi/lib/cjs/cdp/CdpConnection"));',
    '    throw new Error("Private C2 does not support BiDi over CDP without a bound chromium-bidi closure");',
    'unbound optional chromium-bidi protocol refuses before dependency loading', coreName, edits);
  generated[coreName] = replaceUnique(generated[coreName],
    '        const tempDirectories = [];\n        let artifactsDir;\n        if (options.artifactsDir) {\n          artifactsDir = options.artifactsDir;\n        } else {\n          artifactsDir = await import_fs25.default.promises.mkdtemp(import_path25.default.join(import_os10.default.tmpdir(), "playwright-artifacts-"));\n          tempDirectories.push(artifactsDir);\n        }\n        if (userDataDir) {\n          assert(import_path25.default.isAbsolute(userDataDir), "userDataDir must be an absolute path");\n          if (!await existsAsync(userDataDir))\n            await import_fs25.default.promises.mkdir(userDataDir, { recursive: true, mode: 448 });\n        } else {\n          userDataDir = await import_fs25.default.promises.mkdtemp(import_path25.default.join(import_os10.default.tmpdir(), `playwright_${this._name}dev_profile-`));\n          tempDirectories.push(userDataDir);\n        }',
    '        const tempDirectories = [];\n        const route = privateC2.getRuntime().route;\n        const artifactsDir = route.browserArtifacts(options.artifactsDir);\n        if (userDataDir)\n          userDataDir = route.browserProfile(userDataDir, isPersistent);\n        else\n          userDataDir = route.browserProfile(void 0, false);',
    'browser automatic artifacts and profiles use launch-scoped private routes', coreName, edits);
  generated[coreName] = replaceUnique(generated[coreName],
    '      async _deleteAllTempDirs() {\n        await Promise.all(this._tempDirs.map(async (dir) => await import_fs31.default.promises.rm(dir, { recursive: true, force: true }).catch((e) => {\n        })));\n      }',
    '      async _deleteAllTempDirs() {\n        for (const dir of this._tempDirs)\n          privateC2.getRuntime().route.retainAdmittedPath(dir);\n      }',
    'context close retains uploads and downloads instead of recursively removing them', coreName, edits);
  generated[coreName] = replaceUnique(generated[coreName],
    '        this._localPath = localPath;\n        this._unaccessibleErrorMessage = unaccessibleErrorMessage;',
    '        this._localPath = localPath;\n        if (localPath)\n          privateC2.getRuntime().route.admitArtifactIntent(localPath);\n        this._unaccessibleErrorMessage = unaccessibleErrorMessage;',
    'Artifact constructor enrolls the real local writer path before completion', coreName, edits);
  generated[coreName] = replaceUnique(generated[coreName],
    '      async _delete() {\n        if (this._unaccessibleErrorMessage)\n          return;\n        const fileName = await this._localPathAfterFinished();\n        if (this._deleted)\n          return;\n        this._deleted = true;\n        if (fileName)\n          await import_fs12.default.promises.unlink(fileName).catch((e) => {\n          });\n      }',
    '      async _delete() {\n        if (this._unaccessibleErrorMessage || this._deleted)\n          return;\n        const fileName = await this._localPathAfterFinished();\n        this._deleted = true;\n        if (fileName)\n          privateC2.getRuntime().route.retainAdmittedPath(fileName);\n      }',
    'Artifact.delete retains the admitted file without changing product callbacks', coreName, edits);
  generated[coreName] = replaceUnique(generated[coreName],
    '      async reportFinished(error) {\n        if (this._finished)\n          return;\n        this._finished = true;',
    '      async reportFinished(error) {\n        if (this._finished)\n          return;\n        if (this._localPath)\n          privateC2.getRuntime().route.completeArtifactIntent(this._localPath);\n        this._finished = true;',
    'Artifact completion validates the writer result before resolving consumers', coreName, edits);
  generated[coreName] = replaceUnique(generated[coreName],
    '      async deleteOnContextClose() {\n        if (this._deleted)\n          return;\n        this._deleted = true;\n        if (!this._unaccessibleErrorMessage)\n          await import_fs12.default.promises.unlink(this._localPath).catch((e) => {\n          });\n        await this.reportFinished(new TargetClosedError(this.closeReason()));\n      }',
    '      async deleteOnContextClose() {\n        if (this._deleted)\n          return;\n        this._deleted = true;\n        if (!this._unaccessibleErrorMessage)\n          privateC2.getRuntime().route.retainAdmittedPath(this._localPath);\n        await this.reportFinished(new TargetClosedError(this.closeReason()));\n      }',
    'context close retains download data and preserves reportFinished', coreName, edits);
  generated[coreName] = replaceUnique(generated[coreName],
    '      async deleteTmpTracesDir() {\n        if (this._tracesTmpDir)\n          await removeFolders([this._tracesTmpDir]);\n      }',
    '      async deleteTmpTracesDir() {\n        if (this._tracesTmpDir)\n          privateC2.getRuntime().route.retainTraceDirectory(this._tracesTmpDir);\n      }',
    'fallback trace finalizer retains its private directory', coreName, edits);
  generated[coreName] = replaceUnique(generated[coreName],
    '        this._tracesTmpDir = import_fs15.default.mkdtempSync(import_path16.default.join(import_os5.default.tmpdir(), "playwright-tracing-"));',
    '        this._tracesTmpDir = privateC2.getRuntime().route.traceDirectory(createGuid());',
    'fallback trace directory is exclusively enrolled rather than global temporary storage', coreName, edits);
  generated[coreName] = replaceUnique(generated[coreName],
    '        const tmpDir = import_path35.default.join(dir, "upload-" + createGuid());\n        const tempDirWithRootName = params2.rootDirName ? import_path35.default.join(tmpDir, import_path35.default.basename(params2.rootDirName)) : tmpDir;\n        await progress2.race(import_fs37.default.promises.mkdir(tempDirWithRootName, { recursive: true }));\n        this._context._tempDirs.push(tmpDir);',
    '        const tmpDir = privateC2.getRuntime().route.admitUploadDirectory(createGuid());\n        const tempDirWithRootName = privateC2.getRuntime().route.admitUploadRoot(tmpDir, params2.rootDirName);\n        this._context._tempDirs.push(tmpDir);',
    'uploads route under exclusively admitted invocation paths', coreName, edits);
  generated[coreName] = replaceUnique(generated[coreName],
    '            await progress2.race(import_fs37.default.promises.mkdir(import_path35.default.dirname(itemPath), { recursive: true }));\n            const file = import_fs37.default.createWriteStream(itemPath);',
    '            if (privateC2.getRuntime().route.admitUploadFile(tempDirWithRootName, item.name, createGuid()) !== itemPath)\n              throw new Error("Upload writer path changed after admission");\n            const file = import_fs37.default.createWriteStream(itemPath, { flags: "r+" });',
    'upload file stream starts only after its output path is admitted', coreName, edits);
  generated[coreName] = replaceUnique(generated[coreName],
    'async function deleteStackSession(progress2, stackSessions, stacksId) {\n  const session2 = stacksId ? stackSessions.get(stacksId) : void 0;\n  if (!session2)\n    return;\n  await progress2.race(session2.writer);\n  stackSessions.delete(stacksId);\n  if (session2.tmpDir)\n    await progress2.race(removeFolders([session2.tmpDir]));\n}',
    'async function deleteStackSession(progress2, stackSessions, stacksId) {\n  const session2 = stacksId ? stackSessions.get(stacksId) : void 0;\n  if (!session2)\n    return;\n  await progress2.race(session2.writer);\n  stackSessions.delete(stacksId);\n  if (session2.tmpDir)\n    privateC2.getRuntime().route.retainTraceDirectory(session2.tmpDir);\n}',
    'stack trace writer joins before a non-deleting finalizer', coreName, edits);
  generated[coreName] = replaceUnique(generated[coreName],
    '  if (!params2.tracesDir)\n    tmpDir = await progress2.race(import_fs39.default.promises.mkdtemp(import_path37.default.join(import_os18.default.tmpdir(), "playwright-tracing-")));',
    '  if (!params2.tracesDir)\n    tmpDir = privateC2.getRuntime().route.traceDirectory(createGuid());',
    'fallback stack tracing uses enrolled private storage', coreName, edits);

  if (input.dynamicSites?.length) {
    const loaderName = 'frontend/node_modules/playwright/lib/transform/esmLoader.js';
    generated[loaderName] = replaceUnique(generated[loaderName],
      'const { code, map } = babelResult;',
      'const { code: transformedCode, map } = babelResult;\n  const code = privateC2.guardTransformedCode(transformedCode, filename, !!moduleUrl);',
      'Babel-emitted dynamic calls enter the DS23/DS25 loader guards', loaderName, edits);
  }
  const dynamicEdits = applyDynamicSiteEdits(generated, input.sources, input.dynamicSites || []);
  if (input.dynamicSites?.length) {
    generated[commonName] = replaceUnique(generated[commonName],
      'const { code, map } = babelResult;\n  const added = addToCache(code, map, transformData);',
      'const { code: transformedCode, map } = babelResult;\n  const code = privateC2.guardTransformedCode(transformedCode, filename, !!moduleUrl);\n  const added = addToCache(code, map, transformData);',
      'CommonJS and ESM transform results enter the DS23/DS25 loader guards', commonName, edits);
  }
  if (input.dynamicSites?.some(site => site.siteId === 'DS02')) {
    for (const expansion of justifiedDynamicSiteExpansions.filter(item => item.file === commonName))
      generated[commonName] = replaceUnique(generated[commonName], expansion.originalText, expansion.generatedText, expansion.expansionId, commonName, edits);
  }

  const fixtureName = 'frontend/tests/browser/release-resources.spec.ts';
  let fixture = input.releaseFixture;
  fixture = replaceUnique(fixture,
    "import { mkdtemp, rm } from 'node:fs/promises';\n", '',
    'release fixture removes global temporary-directory imports', fixtureName, edits);
  fixture = replaceUnique(fixture,
    "import { tmpdir } from 'node:os';\n", '',
    'release fixture removes global temporary-root import', fixtureName, edits);
  fixture = replaceUnique(fixture,
    "test('normal HTTP caching revalidates unchanged bytes and replaces a repaired wrong response', async ({ playwright, browserName }) => {",
    "test('normal HTTP caching revalidates unchanged bytes and replaces a repaired wrong response', async ({ playwright, browserName }, testInfo) => {",
    'release fixture receives actual TestInfo', fixtureName, edits);
  fixture = replaceUnique(fixture,
    "  const profile = await mkdtemp(join(tmpdir(), 'herdr-revalidation-browser-'));",
    "  const profile = testInfo.outputPath('profiles', 'revalidation');",
    'persistent browser profile is allocated inside the admitted test output', fixtureName, edits);
  fixture = replaceUnique(fixture,
    "    await rm(profile, { recursive: true, force: true });\n", '',
    'release fixture retains persistent profile without recursive deletion', fixtureName, edits);
  generated[fixtureName] = fixture;

  return { generated, edits: [...edits, ...dynamicEdits] };
}

export function sourceClosureEvidence(generated, original) {
  const files = Object.keys(generated).sort();
  return files.map(file => ({
    file,
    originalSHA256: hash(original[file]),
    generatedSHA256: hash(generated[file]),
    originalBytes: Buffer.byteLength(original[file]),
    generatedBytes: Buffer.byteLength(generated[file]),
    originalLines: lineCount(original[file]),
    generatedLines: lineCount(generated[file])
  }));
}

function lineCount(source) {
  return source.length ? source.split('\n').length - (source.endsWith('\n') ? 1 : 0) : 0;
}
