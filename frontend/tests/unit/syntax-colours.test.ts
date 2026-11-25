import { expect, it } from 'vitest';
import { fenceLanguage, fileLanguage, fitsSyntaxBudget, type CodeLanguage, type CodeTokenKind } from '$lib/code-reading';
import { scanCode } from '$lib/syntax-colours';

it.each<[CodeLanguage, string, CodeTokenKind[]]>([
  ['js', 'const name = "<img src=x>"; // inline', ['keyword', 'string', 'comment']],
  ['ts', 'type Key = "blue"; /* comment */', ['keyword', 'string', 'comment']],
  ['go', 'const name = `readme.go` // passthrough', ['keyword', 'string', 'comment']],
  ['py', 'def thing():\n\treturn """a\\\\b""" # exact', ['keyword', 'keyword', 'string', 'comment']],
  ['json', '{"ready":true,"length":123.4e+2}', ['string', 'keyword', 'string', 'number']],
  ['sh', 'if [ "$count" = 12 ]; then # yes', ['keyword', 'string', 'number', 'keyword', 'comment']],
])('scans %s as exact text slices', (language, source, kinds) => {
  const tokens = scanCode(source, language);
  expect(tokens.map(token => token.text).join('')).toBe(source);
  expect(tokens.flatMap(token => token.kind ?? [])).toEqual(kinds);
});

it.each([
  ['js', '"', '\n'],
  ['ts', "'", '\r\n'],
  ['py', '"', '\\\n'],
  ['py', "'''", '\r'],
  ['go', '`', '\u2028'],
  ['json', '"', '\n'],
  ['sh', "'", '\n'],
  ['js', '/*', '\u2029'],
] as const)('leaves an unclosed %s %s delimiter plain without affecting the next line', (language, opener, end) => {
  const unfinished = `${opener}unfinished 17${end}`;
  const source = `${unfinished}42 "yes"`;
  const tokens = scanCode(source, language);
  expect(tokens.map(token => token.text).join('')).toBe(source);
  expect(tokens.filter(token => token.kind)).toEqual([
    { text: '42', kind: 'number' }, { text: '"yes"', kind: 'string' },
  ]);
});

const newline = /\r\n?|[\n\u2028\u2029]/u;

function expectLineScoped(language: CodeLanguage, source: string) {
  const tokens = scanCode(source, language);
  const colours = tokens.filter(token => token.kind);
  expect(tokens.map(token => token.text).join('')).toBe(source);
  expect(colours.every(token => !newline.test(token.text))).toBe(true);
  expect(colours).toEqual(source.split(newline).flatMap(line => scanCode(line, language).filter(token => token.kind)));
}

it.each<[CodeLanguage, string]>([
  ['js', 'const invalidName = /[/*?]/;\nconst count = 42; /* comment */'],
  ['js', 'export default /`/g;\nconst count = 42;\nconst label = `ok`;'],
  ['ts', 'export default /[/*?]/;\nconst count = 42; /* real comment */'],
  ['js', 'import match from "pkg"\n/[/*?]/.test(match)\nconst count = 42;'],
  ['js', 'const label = `before ${format(`nested`)}\nafter`;\nconst count = 42;'],
  ['ts', 'const text = "first\\\r\n/*second";\nconst count = 42; /* real comment */'],
  ['js', 'const count = 12 / 3; /* still a\ncomment */\nreturn /prefix[/*?]/.test(name);'],
  ['js', 'const ratio = dividend / "n/g"; const label = "ok";'],
  ['js', '/* unfinished\nconst count = 42;'],
  ['js', '`first\n42`'],
  ['go', 'const name = `read\nme.go` // passthrough'],
  ['py', 'def thing():\n\treturn """a\n\\\\b""" # exact'],
  ['py', 'text = "first\\\nsecond"\ncount = 42 # independent'],
  ['sh', 'echo $# ${#name} tag#part escaped\\ #word continued\\\n#word; printf \'%s\' "$1";# comment'],
  ['sh', "cat <<EOF\nDon't stop\nEOF\necho 'one'; echo 'two'"],
  ['sh', "cat <<\\EOF\nDon't stop\nEOF\necho 'one'; echo 'two'"],
  ['sh', "cat <<-'EOF'\nDon't stop\n\tEOF\necho 'one'; echo 'two'"],
  ['sh', "x='one\ntwo'; echo 42; y='done'"],
  ['sh', 'x=prefix"one\ntwo "; echo 42; y="done"'],
  ['sh', "separator=" + String.raw`'\' $'one\'two' \$'\'` + "\n# Normalize path separators\nprintf '%s' \"$separator\""],
])('keeps %s sample line-scoped: %j', expectLineScoped);

it('treats hashes inside Bash words as plain', () => {
  const source = 'echo $# ${#name} tag#part escaped\\ #word; # comment';
  expect(scanCode(source, 'sh').filter(token => token.kind)).toEqual([{ text: '# comment', kind: 'comment' }]);
});

it.each<CodeLanguage>(['js', 'ts', 'go', 'py', 'json', 'sh'])('round-trips every generated %s input with independent lines', language => {
  const pieces = ['', '/*', '*/', '//', '#', '/', '$', '\\', "'", '"', '"""', '`', '\t', '\n', '\r', '42', 'const', 'é😀', '\ud800'];
  for (const first of pieces) {
    for (const second of pieces) {
      for (const third of pieces) expectLineScoped(language, first + second + third);
    }
  }
});

it('accepts only explicit languages and extensions', () => {
  expect(fenceLanguage('JavaScript')).toBe('js');
  expect(fenceLanguage('PYTHON')).toBe('py');
  expect(fenceLanguage('shell')).toBeUndefined();
  expect(fileLanguage('/path/to/test.CJS')).toBe('js');
  expect(fileLanguage('/path/example.go/file')).toBeUndefined();
  expect(fileLanguage('program.ts.backup')).toBeUndefined();
});

it('keeps escaped quotes plain after an unclosed JS quote near the size limit', () => {
  const source = '"' + String.raw`\"`.repeat(8191);
  expect(fitsSyntaxBudget(source)).toBe(true);
  expect(scanCode(source, 'js')).toEqual([{ text: source }]);
});

it('scans a long sequence of slashes without affecting the next line', () => {
  const source = '[/'.repeat(8000) + '\nconst closed = /[/*?]/;\nconst count = 42;';
  expect(fitsSyntaxBudget(source)).toBe(true);
  expectLineScoped('js', source);
});

it.each(['\n', '\r\n', '\r', '\u2028', '\u2029'])('accepts 300 lines but not 301 with separator %j', separator => {
  const atLimit = Array(300).fill('42;').join(separator);
  expect(fitsSyntaxBudget(atLimit)).toBe(true);
  expect(fitsSyntaxBudget(`${atLimit}${separator}42;`)).toBe(false);
});

it('bounds UTF-8 bytes, including edge cases and unterminated input', () => {
  const source = '`\t' + '😀'.repeat(4095) + '\n\n';
  expect(fitsSyntaxBudget(source)).toBe(true);
  expect(fitsSyntaxBudget(source + 'a')).toBe(false);
  expectLineScoped('js', source);
  expectLineScoped('go', ' \n\n\t');
});
