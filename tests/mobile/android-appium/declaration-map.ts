import assert from 'node:assert/strict';
import ts from 'typescript';

const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';
type Segment = number[];

function decode(text: string): Segment[][] {
  let source = 0, line = 0, column = 0, name = 0;
  return text.split(';').map(row => {
    let generated = 0;
    return row ? row.split(',').map(segment => {
      const values: number[] = [];
      let value = 0, shift = 0;
      for (const char of segment) {
        const digit = alphabet.indexOf(char);
        assert.ok(digit >= 0);
        value += (digit & 31) * 2 ** shift;
        if (digit & 32) { shift += 5; continue; }
        values.push(value & 1 ? -(value >> 1) : value >> 1);
        value = shift = 0;
      }
      assert.equal(shift, 0);
      assert.ok([1, 4, 5].includes(values.length));
      generated += values[0];
      if (values.length === 1) return [generated];
      source += values[1]; line += values[2]; column += values[3];
      const result = [generated, source, line, column];
      if (values.length === 5) { name += values[4]; result.push(name); }
      return result;
    }) : [];
  });
}

function vlq(n: number): string {
  let value = n < 0 ? -n * 2 + 1 : n * 2;
  let result = '';
  do {
    let digit = value % 32;
    value = Math.floor(value / 32);
    if (value) digit |= 32;
    result += alphabet[digit];
  } while (value);
  return result;
}

function encode(rows: Segment[][]): string {
  let source = 0, line = 0, column = 0, name = 0;
  return rows.map(row => {
    let generated = 0;
    return row.map(segment => {
      const values = [segment[0] - generated];
      generated = segment[0];
      if (segment.length > 1) {
        values.push(segment[1] - source, segment[2] - line, segment[3] - column);
        [source, line, column] = segment.slice(1, 4);
        if (segment.length === 5) { values.push(segment[4] - name); name = segment[4]; }
      }
      return values.map(vlq).join('');
    }).join(',');
  }).join(';');
}

function position(text: string, line: number, column: number): number {
  const lines = text.split('\n');
  assert.ok(line >= 0 && line < lines.length);
  assert.ok(column >= 0 && column <= lines[line].length);
  return lines.slice(0, line).reduce((sum, row) => sum + row.length + 1, 0) + column;
}

function surface(text: string): string[] {
  const source = ts.createSourceFile('input.ts', text, ts.ScriptTarget.Latest, true);
  return source.statements.filter(node =>
    (ts.canHaveModifiers(node) && ts.getModifiers(node)?.some(modifier => modifier.kind === ts.SyntaxKind.ExportKeyword))
    || ts.isImportDeclaration(node)).map(node => {
    if (!ts.isFunctionDeclaration(node)) return node.getText(source);
    assert.ok(node.type && node.body, 'Exported function must have an explicit return type and body');
    return text.slice(node.getStart(source), node.body.getStart(source));
  });
}

export function relocateDeclarationMap(
  name: string, before: string, after: string, declaration: string, mapText: string,
  edits: Array<[string, string]>,
): string {
  let current = before;
  const identities: Array<number | null> = Array.from({ length: before.length }, (_, index) => index);
  for (const [from, to] of edits) {
    const at = current.indexOf(from);
    assert.ok(at >= 0 && current.indexOf(from, at + 1) < 0);
    let prefix = 0, suffix = 0;
    while (prefix < Math.min(from.length, to.length) && from[prefix] === to[prefix]) prefix++;
    while (suffix < Math.min(from.length, to.length) - prefix && from.at(-suffix - 1) === to.at(-suffix - 1)) suffix++;
    identities.splice(at, from.length, ...identities.slice(at, at + prefix),
      ...Array<number | null>(to.length - prefix - suffix).fill(null),
      ...identities.slice(at + from.length - suffix, at + from.length));
    current = current.slice(0, at) + to + current.slice(at + from.length);
  }
  assert.equal(current, after);
  assert.deepEqual(surface(before), surface(after));
  const inverse = new Map<number, number>();
  identities.forEach((old, index) => { if (old !== null) inverse.set(old, index); });
  inverse.set(before.length, after.length);
  const map = JSON.parse(mapText);
  assert.equal(map.file, `${name}.d.ts`);
  assert.deepEqual(map.sources, [`../../../../lib/commands/context/${name}.ts`]);
  assert.ok(!map.sourceRoot);
  if (map.sourcesContent) assert.deepEqual(map.sourcesContent, [before]);
  const rows = decode(map.mappings);
  assert.equal(encode(rows), map.mappings);
  const relocated = rows.map((row, generatedLine) => row.map(segment => {
    position(declaration, generatedLine, segment[0]);
    if (segment.length === 1) return segment;
    assert.equal(segment[1], 0);
    const old = position(before, segment[2], segment[3]);
    const next = inverse.get(old);
    assert.notEqual(next, undefined, 'Declaration mapping points into changed source');
    assert.equal(before[old], after[next!]);
    const lines = after.slice(0, next!).split('\n');
    const location = [lines.length - 1, lines.at(-1)!.length];
    assert.equal(position(after, location[0], location[1]), next);
    return [segment[0], 0, ...location, ...segment.slice(4)];
  }));
  map.mappings = encode(relocated);
  assert.deepEqual(decode(map.mappings), relocated);
  if (map.sourcesContent) map.sourcesContent = [after];
  assert.equal(declaration.match(/sourceMappingURL=(\S+)/)?.[1], `${name}.d.ts.map`);
  return JSON.stringify(map) + '\n';
}
