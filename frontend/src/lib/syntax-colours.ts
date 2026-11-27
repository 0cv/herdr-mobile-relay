import { codeLineBreak, type CodeLanguage, type CodeToken, type CodeTokenKind } from './code-reading';

const js = 'async await break case catch class const continue debugger default delete do else export extends false finally for function if import in instanceof let new null of return static super switch this throw true try typeof undefined var void while with yield';
const keywords: Record<CodeLanguage, Set<string>> = {
  js: new Set(js.split(' ')),
  ts: new Set((js + ' abstract any as boolean declare enum implements infer interface keyof namespace never number override private protected public readonly satisfies string type unknown').split(' ')),
  go: new Set('break case chan const continue default defer else fallthrough false for func go goto if import interface map nil package range return select struct switch true type var'.split(' ')),
  py: new Set('and as assert async await break class continue def del elif else except False finally for from global if import in is lambda None nonlocal not or pass raise return True try while with yield'.split(' ')),
  json: new Set(['true', 'false', 'null']),
  sh: new Set('case do done elif else esac export fi for function if in local readonly return select set then time until while'.split(' ')),
};

const word = /[\p{ID_Start}_$][\p{ID_Continue}$]*/uy;
const number = /(?:0[xX][0-9a-fA-F_]+|0[bB][01_]+|0[oO][0-7_]+|\d[\d_]*(?:\.\d[\d_]*)?(?:[eE][+-]?\d[\d_]*)?)/uy;

function quotedEnd(line: string, start: number, delimiter: string, raw: boolean): number {
  for (let cursor = start + delimiter.length; cursor < line.length; cursor++) {
    if (line.startsWith(delimiter, cursor)) return cursor + delimiter.length;
    if (!raw && line[cursor] === '\\') cursor++;
  }
  return -1;
}

function scanLine(line: string, language: CodeLanguage, tokens: CodeToken[]): void {
  const slashComments = language === 'js' || language === 'ts' || language === 'go';
  const bash = language === 'sh';
  let shellWordStart = true;
  let plain = 0;
  let cursor = 0;
  while (cursor < line.length) {
    const start = cursor;
    const first = line[cursor++];
    const next = line[cursor];
    const hashComment = first === '#' && (language === 'py' || (bash && shellWordStart));
    if (bash) shellWordStart = /[ \t|&;()<>]/u.test(first);
    let kind: CodeTokenKind | undefined;
    if (hashComment || (slashComments && first === '/' && next === '/')) {
      cursor = line.length;
      kind = 'comment';
    } else if (slashComments && first === '/' && next === '*') {
      const end = line.indexOf('*/', cursor + 1);
      if (end < 0) break;
      cursor = end + 2;
      kind = 'comment';
    } else if (first === '"' || (first === "'" && language !== 'json') || (first === '`' && slashComments)) {
      const delimiter = language === 'py' && line.startsWith(first.repeat(3), start) ? first.repeat(3) : first;
      const raw = (language === 'go' && first === '`') || (bash && first === "'");
      const end = quotedEnd(line, start, delimiter, raw);
      if (end < 0) break;
      cursor = end;
      kind = 'string';
    } else if (bash && first === '\\') {
      cursor++;
    } else {
      word.lastIndex = number.lastIndex = start;
      const identifier = word.exec(line)?.[0];
      if (identifier) {
        cursor = word.lastIndex;
        if (keywords[language].has(identifier)) kind = 'keyword';
      } else if (number.test(line)) {
        cursor = number.lastIndex;
        kind = 'number';
      }
    }
    if (!kind) continue;
    if (plain < start) tokens.push({ text: line.slice(plain, start) });
    tokens.push({ text: line.slice(start, cursor), kind });
    plain = cursor;
  }
  if (plain < line.length) tokens.push({ text: line.slice(plain) });
}

export function scanCode(text: string, language: CodeLanguage): CodeToken[] {
  const parts = text.split(codeLineBreak);
  const tokens: CodeToken[] = [];
  for (let i = 0; i < parts.length; i += 2) {
    scanLine(parts[i], language, tokens);
    if (i + 1 < parts.length) tokens.push({ text: parts[i + 1] });
  }
  return tokens;
}
