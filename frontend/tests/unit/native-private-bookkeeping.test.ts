import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import ts from 'typescript';
import { describe, expect, it } from 'vitest';
import { compactPrivateBookkeeping, nativePrivateBookkeeping } from '../../scripts/native-private-bookkeeping';

function execute(source: string) {
  const code = ts.transpileModule(source, {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS },
  }).outputText;
  const exports: Record<string, any> = {};
  new Function('exports', code)(exports);
  return exports;
}

// Reversing only the private representation must recover the original AST.
// This detects changed data keys, expressions, guards, ordering and deadlines
// across the actual two modules, rather than relying solely on toy fixtures.
function canonical(source: string, filename: string, undo = false) {
  const file = ts.createSourceFile(filename, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
  const result = ts.transform(file, [(context) => {
    const visit: ts.Visitor = (node) => {
      const updated = ts.visitEachChild(node, visit, context);
      if (!undo) return updated;
      const f = context.factory;
      if (ts.isPropertyAccessExpression(updated) && ts.isPrivateIdentifier(updated.name)) {
        const name = f.createIdentifier(updated.name.text.slice(1));
        return ts.isPropertyAccessChain(updated)
          ? f.updatePropertyAccessChain(updated, updated.expression, updated.questionDotToken, name)
          : f.updatePropertyAccessExpression(updated, updated.expression, name);
      }
      if ((ts.isPropertyDeclaration(updated) || ts.isMethodDeclaration(updated)) && ts.isPrivateIdentifier(updated.name)) {
        const modifiers = [f.createModifier(ts.SyntaxKind.PrivateKeyword), ...(updated.modifiers ?? [])];
        const name = f.createIdentifier(updated.name.text.slice(1));
        return ts.isPropertyDeclaration(updated)
          ? f.updatePropertyDeclaration(updated, modifiers, name, updated.questionToken ?? updated.exclamationToken, updated.type, updated.initializer)
          : f.updateMethodDeclaration(updated, modifiers, updated.asteriskToken, name, updated.questionToken,
            updated.typeParameters, updated.parameters, updated.type, updated.body);
      }
      return updated;
    };
    return (root) => ts.visitNode(root, visit) as ts.SourceFile;
  }]);
  try {
    const printed = ts.createPrinter({ removeComments: true }).printFile(result.transformed[0]);
    const parsed = ts.createSourceFile(filename, printed, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
    const fingerprint = (node: ts.Node): unknown => {
      const children: unknown[] = [];
      ts.forEachChild(node, (child) => { children.push(fingerprint(child)); });
      const literal = ts.isIdentifier(node) || ts.isLiteralExpression(node)
        || ts.isTemplateHead(node) || ts.isTemplateMiddle(node) || ts.isTemplateTail(node);
      return [node.kind, node.flags & (ts.NodeFlags.Const | ts.NodeFlags.Let | ts.NodeFlags.OptionalChain),
        literal ? node.text : null, 'rawText' in node ? node.rawText : null, children];
    };
    return JSON.stringify(fingerprint(parsed));
  } finally { result.dispose(); }
}

const fixture = `
export class Model {
  private generation = 0;
  private readonly memory = new Map<string, number>();
  readonly publicState = { generation: 0 };
  constructor(private readonly options: { epoch: string }) {}
  private update(value: { generation: number }) {
    this.generation += value.generation;
    this.memory.set(this.options.epoch, this.generation);
    return { generation: this.generation, epoch: this.options.epoch };
  }
  run(value: { generation: number }) { return this.update(value); }
  snapshot() { return Array.from(this.memory); }
}
`;

describe('native private bookkeeping representation', () => {
  it('keeps public calls, constructor options, foreign properties and serialized keys unchanged', () => {
    const transformed = compactPrivateBookkeeping(fixture, 'fixture.ts', 'Model')!;
    expect(transformed).toContain('#generation');
    expect(transformed).toContain('private readonly options');
    const Original = execute(fixture).Model;
    const Native = execute(transformed).Model;
    const original = new Original({ epoch: 'opaque' });
    const native = new Native({ epoch: 'opaque' });
    for (const generation of [1, 2, -1]) {
      expect(native.run({ generation })).toEqual(original.run({ generation }));
      expect(native.snapshot()).toEqual(original.snapshot());
    }
    expect(native.publicState).toEqual(original.publicState);
    expect(canonical(transformed, 'fixture.ts', true)).toBe(canonical(fixture, 'fixture.ts'));
  });

  it('retains the existing raw upload test seam and its generic signature', () => {
    const source = 'export class Model { private count = 0; private draining = false; private sendUploadRequest<T>(value: T): T { this.count++; return value; } }';
    const transformed = compactPrivateBookkeeping(source, 'fixture.ts', 'Model')!;
    expect(transformed).toContain('private sendUploadRequest<T>');
    expect(transformed).toContain('private draining = false');
    const Native = execute(transformed).Model;
    const native = new Native();
    expect(native.sendUploadRequest({ target: 'old', upload_id: 'owned' })).toEqual({ target: 'old', upload_id: 'owned' });
  });

  it('does not modify another class or a nested class with its own this receiver', () => {
    const source = 'export class Other { private value = 5; } export class Model { private value = 1; run() { return new (class { value = 9; run() { return this.value; } })().run() + this.value; } }';
    const transformed = compactPrivateBookkeeping(source, 'fixture.ts', 'Model')!;
    expect(transformed).toContain('private value = 5');
    expect(new (execute(transformed).Model)().run()).toBe(10);
    expect(canonical(transformed, 'fixture.ts', true)).toBe(canonical(source, 'fixture.ts'));
  });

  it('handles literal indexed and optional private access without changing data literals', () => {
    const source = 'export class Model { private value = 4; run() { return [this["value"], this?.value, { value: "value" }]; } }';
    const transformed = compactPrivateBookkeeping(source, 'fixture.ts', 'Model')!;
    expect(new (execute(transformed).Model)().run()).toEqual([4, 4, { value: 'value' }]);
  });

  it('refuses dynamic indexed this access rather than silently changing its meaning', () => {
    expect(() => compactPrivateBookkeeping('class Model { private value = 1; run(key: string) { return this[key]; } }', 'fixture.ts', 'Model'))
      .toThrow('dynamic this access needs an explicit private-member audit');
  });

  it('leaves public-only or absent target classes unchanged', () => {
    expect(compactPrivateBookkeeping('class Model { value = 1; }', 'fixture.ts', 'Model')).toBeNull();
    expect(compactPrivateBookkeeping(fixture, 'fixture.ts', 'Absent')).toBeNull();
  });

  it('limits the plugin to the two audited source modules, not arbitrary imports', () => {
    const plugin = nativePrivateBookkeeping();
    expect(plugin.enforce).toBe('pre');
    const transform = plugin.transform as (source: string, id: string) => { code: string } | null;
    expect(transform(fixture, '/frontend/src/lib/unrelated.ts')).toBeNull();
    expect(transform(fixture, '/frontend/src/lib/store.ts?raw')).toBeNull();
  });

  it.each(['store', 'last-known'])('only changes private representation in actual %s source', (module) => {
    const path = resolve(import.meta.dirname, '../../src/lib', `${module}.ts`);
    const source = readFileSync(path, 'utf8');
    const className = module === 'store' ? 'RelayStore' : 'LastKnownSessionCache';
    const transformed = compactPrivateBookkeeping(source, path, className)!;
    expect(transformed).toContain('#');
    expect(canonical(transformed, path, true)).toBe(canonical(source, path));
  });
});
