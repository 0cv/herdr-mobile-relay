export type CodeLanguage = 'js' | 'ts' | 'go' | 'py' | 'json' | 'sh';
export type CodeTokenKind = 'keyword' | 'string' | 'number' | 'comment';
export interface CodeToken { text: string; kind?: CodeTokenKind }
export type ScanCode = (text: string, language: CodeLanguage) => CodeToken[];
export type LoadSyntax = () => Promise<ScanCode | null>;

export function loadCodeColours(node: HTMLElement | undefined, loadSyntax: LoadSyntax, paint: (scan: ScanCode) => void): () => void {
  let active = true;
  let update: (() => void) | undefined;
  void loadSyntax().then(scan => {
    if (!active || !scan) return;
    update = () => {
      const selection = node && window.getSelection();
      if (node && selection && !selection.isCollapsed && selection.rangeCount && selection.getRangeAt(0).intersectsNode(node)) return;
      document.removeEventListener('selectionchange', update!);
      paint(scan);
    };
    document.addEventListener('selectionchange', update);
    update();
  });
  return () => {
    active = false;
    if (update) document.removeEventListener('selectionchange', update);
  };
}

const fences = new Map<string, CodeLanguage>([
  ['js', 'js'], ['javascript', 'js'], ['ts', 'ts'], ['typescript', 'ts'],
  ['go', 'go'], ['py', 'py'], ['python', 'py'], ['json', 'json'], ['sh', 'sh'], ['bash', 'sh'],
]);

const extensions = new Map<string, CodeLanguage>([
  ['js', 'js'], ['mjs', 'js'], ['cjs', 'js'], ['ts', 'ts'], ['go', 'go'],
  ['py', 'py'], ['json', 'json'], ['sh', 'sh'], ['bash', 'sh'],
]);

export function fenceLanguage(label: string): CodeLanguage | undefined {
  return fences.get(label.toLowerCase());
}

export function fileLanguage(path: string): CodeLanguage | undefined {
  const dot = path.lastIndexOf('.');
  return dot < 0 ? undefined : extensions.get(path.slice(dot + 1).toLowerCase());
}

const maxBytes = 16 * 1024;
export const codeLineBreak = /(\r\n?|[\n\u2028\u2029])/gu;

export function fitsSyntaxBudget(text: string): boolean {
  if (text.length > maxBytes) return false;
  let lines = 1;
  const newline = new RegExp(codeLineBreak);
  while (newline.exec(text)) {
    if (++lines > 300) return false;
  }
  return new TextEncoder().encode(text).length <= maxBytes;
}
