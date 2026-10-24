export function terminalTextError(text: string): string {
  if (!text) return 'Enter text first.';
  if (new TextEncoder().encode(text).length > 65_536) return 'Text exceeds 65,536 UTF-8 bytes.';
  if (/[\u0000-\u001f\u007f-\u009f\u2028\u2029]/u.test(text)
    || Array.from(text).some(value => value.codePointAt(0)! >= 0xd800 && value.codePointAt(0)! <= 0xdfff)) {
    return 'Use one line without control characters. Send Enter separately.';
  }
  return '';
}
