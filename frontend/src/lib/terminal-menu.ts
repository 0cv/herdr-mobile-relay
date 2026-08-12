export interface TerminalMenuAction {
  label: string;
  keys: string[];
  cancel: boolean;
}

export interface TerminalMenu {
  signature: string;
  title: string;
  actions: TerminalMenuAction[];
}

const KEY_NAME: Record<string, string> = {
  enter: 'Enter',
  return: 'Enter',
  esc: 'Escape',
  escape: 'Escape',
  tab: 'Tab',
  space: ' ',
  spacebar: ' ',
  '↑': 'Up',
  '↓': 'Down',
  '←': 'Left',
  '→': 'Right',
  y: 'y',
  n: 'n',
};

const ACTION_LABEL: Record<string, string> = {
  accept: 'Accept',
  back: 'Back',
  cancel: 'Cancel',
  choose: 'Choose',
  close: 'Close',
  confirm: 'Confirm',
  continue: 'Continue',
  deny: 'Deny',
  down: 'Down',
  move: 'Move',
  navigate: 'Navigate',
  next: 'Next',
  no: 'No',
  previous: 'Previous',
  quit: 'Quit',
  select: 'Select',
  submit: 'Submit',
  toggle: 'Toggle',
  up: 'Up',
  yes: 'Yes',
};

const KEY_TOKEN = String.raw`(?:Ctrl\+[A-Za-z]|Alt\+[A-Za-z]|Shift\+[A-Za-z]|Enter|Return|Esc(?:ape)?|Tab|Space(?:bar)?|↑|↓|←|→)`;
const VERB_TOKEN = Object.keys(ACTION_LABEL).join('|');
const SINGLE_HINT = new RegExp(`(${KEY_TOKEN})\\s*(?:to|:|=|-)?\\s*(${VERB_TOKEN})\\b`, 'giu');
const PAIRED_ARROWS = /([↑↓←→])\s*[/|]\s*([↑↓←→])\s*(?:to|:|=|-)?\s*(navigate|move|select|choose|previous|next)?/giu;
const YES_NO = /\b(?:press\s+)?([yn])\s*[/|]\s*([yn])\b/iu;
const EXPLICIT_LETTER = /\b([yn])\s*(?:to|:|=|-)+\s*(yes|no|accept|deny|confirm|cancel)\b/giu;

function normalizeKey(value: string): string {
  const lower = value.toLocaleLowerCase();
  if (KEY_NAME[lower]) return KEY_NAME[lower];
  const modifier = value.match(/^(Ctrl|Alt|Shift)\+([A-Za-z])$/iu);
  if (modifier) return `${modifier[1][0].toUpperCase()}${modifier[1].slice(1).toLowerCase()}+${modifier[2].toUpperCase()}`;
  return '';
}

function addAction(actions: TerminalMenuAction[], key: string, verb: string) {
  const normalized = normalizeKey(key);
  const label = ACTION_LABEL[verb.toLocaleLowerCase()] || '';
  if (!normalized || !label || actions.some((action) => action.keys[0] === normalized)) return;
  actions.push({
    label: label === 'Navigate' || label === 'Move' ? normalized.replace('Arrow', '') : label,
    keys: [normalized],
    cancel: ['Cancel', 'Close', 'Quit', 'Back'].includes(label),
  });
}

function cleanTitle(value: string): string {
  return value
    .replace(/[│┃║]/gu, ' ')
    .replace(/^[\s┌┐└┘╭╮╰╯─━═*#>[\]-]+|[\s┌┐└┘╭╮╰╯─━═*#<[\]-]+$/gu, '')
    .replace(/\s+/g, ' ')
    .trim()
    .slice(0, 100);
}

export function detectTerminalMenu(value: string): TerminalMenu | null {
  const lines = value.replace(/\r\n?/g, '\n').split('\n');
  while (lines.length && !lines.at(-1)?.trim()) lines.pop();
  const tail = lines.slice(-10);
  if (!tail.length) return null;
  const candidateLines = tail.slice(-4);
  const footer = candidateLines.join(' · ');
  const actions: TerminalMenuAction[] = [];

  for (const match of footer.matchAll(PAIRED_ARROWS)) {
    const fallback = match[1] === '↑' ? 'up' : match[1] === '↓' ? 'down' : match[1] === '←' ? 'previous' : 'next';
    addAction(actions, match[1], match[3] || fallback);
    const second = match[2] === '↑' ? 'up' : match[2] === '↓' ? 'down' : match[2] === '←' ? 'previous' : 'next';
    addAction(actions, match[2], match[3] || second);
  }
  for (const match of footer.matchAll(SINGLE_HINT)) addAction(actions, match[1], match[2]);
  for (const match of footer.matchAll(EXPLICIT_LETTER)) addAction(actions, match[1], match[2]);
  const yesNo = footer.match(YES_NO);
  if (yesNo) {
    addAction(actions, yesNo[1], yesNo[1].toLocaleLowerCase() === 'y' ? 'yes' : 'no');
    addAction(actions, yesNo[2], yesNo[2].toLocaleLowerCase() === 'y' ? 'yes' : 'no');
  }

  if (!actions.length) return null;
  const hintIndex = tail.findIndex((line) => {
    SINGLE_HINT.lastIndex = 0;
    PAIRED_ARROWS.lastIndex = 0;
    EXPLICIT_LETTER.lastIndex = 0;
    return SINGLE_HINT.test(line) || PAIRED_ARROWS.test(line) || EXPLICIT_LETTER.test(line) || YES_NO.test(line);
  });
  if (hintIndex < Math.max(0, tail.length - 4)) return null;
  const titleCandidates = tail.slice(0, hintIndex).map(cleanTitle).filter(Boolean);
  const title = titleCandidates.at(-1) || 'Terminal menu';
  const signature = `${title}\u0000${candidateLines.join('\n')}\u0000${actions.map((action) => action.keys.join('+')).join(',')}`;
  return { signature, title, actions };
}
