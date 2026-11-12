export function pathBase(path: string): string {
  return path.replace(/[\\/]+$/, '').split(/[\\/]/).filter(Boolean).at(-1) || '';
}
