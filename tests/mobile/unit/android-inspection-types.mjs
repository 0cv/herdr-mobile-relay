import assert from 'node:assert/strict';
import {createRequire} from 'node:module';
import {fileURLToPath} from 'node:url';

const require = createRequire(import.meta.url);
const ts = require('typescript');

export function resultValidator(declaration) {
  const program = ts.createProgram([declaration], {
    module: ts.ModuleKind.NodeNext, target: ts.ScriptTarget.ES2022, skipLibCheck: true,
    types: ['node'], typeRoots: [fileURLToPath(new URL('../node_modules/@types', import.meta.url))],
  });
  assert.deepEqual(ts.getPreEmitDiagnostics(program).map(value => ts.flattenDiagnosticMessageText(value.messageText, '\n')), []);
  const checker = program.getTypeChecker();
  const source = program.getSourceFile(declaration);
  const exported = checker.getExportsOfModule(checker.getSymbolAtLocation(source));
  const symbol = exported.find(entry => entry.name === 'RetainedInspectionResult');
  assert.ok(symbol, 'Installed route result declaration must exist');
  const resultType = checker.getDeclaredTypeOfSymbol(symbol);
  function validate(value, type, location) {
    if (type.isUnion()) {
      const accepted = type.types.some(candidate => { try { validate(value, candidate, location); return true; } catch { return false; } });
      assert.ok(accepted, `${location}: declared union mismatch`);
      return;
    }
    if (type.flags & ts.TypeFlags.NumberLiteral) { assert.equal(value, type.value, location); return; }
    if (type.flags & ts.TypeFlags.StringLiteral) { assert.equal(value, type.value, location); return; }
    if (type.flags & ts.TypeFlags.BooleanLiteral) { assert.equal(value, type.intrinsicName === 'true', location); return; }
    if (type.flags & ts.TypeFlags.String) { assert.equal(typeof value, 'string', location); return; }
    if (type.flags & ts.TypeFlags.Number) { assert.ok(Number.isFinite(value), location); return; }
    if (checker.isArrayType(type)) {
      assert.ok(Array.isArray(value), location);
      for (const entry of value) validate(entry, checker.getTypeArguments(type)[0], `${location}[]`);
      return;
    }
    assert.ok(type.flags & ts.TypeFlags.Object, `${location}: unvalidated declaration type ${checker.typeToString(type)}`);
    assert.ok(value && typeof value === 'object' && !Array.isArray(value), location);
    const properties = checker.getPropertiesOfType(type);
    assert.deepEqual(Object.keys(value).sort(), properties.map(property => property.name).sort(), `${location}: declared fields`);
    for (const property of properties) validate(value[property.name], checker.getTypeOfSymbolAtLocation(property, property.valueDeclaration ?? property.declarations[0]), `${location}.${property.name}`);
  }
  return value => validate(value, resultType, 'RetainedInspectionResult');
}
