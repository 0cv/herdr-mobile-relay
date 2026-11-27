export const defaultKeyRow: readonly string[] = ['Escape', 'Tab', 'shift', 'ctrl', 'alt', 'fkeys', 'arrows', 'Enter'];
export const ctrlLetters = [...'ABCDEFGHJKLNOPQRSTUVWXYZ'];
export const keyRowEntries = [...defaultKeyRow, 'Up', 'Down', 'Left', 'Right', 'shift+tab',
  ...ctrlLetters.map((letter) => `ctrl+${letter.toLowerCase()}`),
];

export function keyRowLabel(id: string): string {
  switch (id) {
    case 'Escape': return 'Esc';
    case 'fkeys': return 'F keys';
    case 'arrows': return 'Arrow keys';
    case 'shift+tab': return 'Shift+Tab';
    default: return id.startsWith('ctrl+')
      ? `Ctrl+${id.slice(-1).toUpperCase()}`
      : id[0].toUpperCase() + id.slice(1);
  }
}

export function validKeyRow(value: unknown): value is string[] {
  return Array.isArray(value) && new Set(value).size === value.length
    && value.every((id) => keyRowEntries.includes(id));
}

export function isDefaultKeyRow(row: readonly string[]): boolean {
  return row.length === defaultKeyRow.length && row.every((id, index) => id === defaultKeyRow[index]);
}
