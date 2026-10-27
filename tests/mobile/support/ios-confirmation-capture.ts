export const IOS_CONFIRMATION_CAPTURE_LIMITS = Object.freeze({
  inputUnits: 262_144, nodes: 512, depth: 64, projectionBytes: 48 * 1024, evidenceBytes: 64 * 1024, localMs: 1_000,
});

const nativeTypes = Object.freeze([
  'unknown',
  'XCUIElementTypeApplication', 'XCUIElementTypeWindow', 'XCUIElementTypeOther',
  'XCUIElementTypeNavigationBar', 'XCUIElementTypeAlert', 'XCUIElementTypeSheet', 'XCUIElementTypeToolbar',
  'XCUIElementTypeButton', 'XCUIElementTypeStaticText', 'XCUIElementTypeTextField', 'XCUIElementTypeSecureTextField',
  'XCUIElementTypeTextView', 'XCUIElementTypeSearchField', 'XCUIElementTypeScrollView', 'XCUIElementTypeCollectionView',
  'XCUIElementTypeTable', 'XCUIElementTypeCell', 'XCUIElementTypeWebView', 'XCUIElementTypeImage',
  'XCUIElementTypeSwitch', 'XCUIElementTypeKeyboard',
]);
const textMatches = Object.freeze(['missing', 'withheld', 'Add', 'Add to Home Screen', 'Cancel']);
const flags = Object.freeze(['unknown', false, true]);
const format = Object.freeze({
  version: 2,
  encoding: 'json-array-rows',
  columns: Object.freeze(['parent', 'depth', 'type', 'name', 'label', 'value', 'visible', 'enabled', 'hittable', 'accessible', 'x', 'y', 'width', 'height']),
  enums: Object.freeze({
    type: nativeTypes, name: textMatches, label: textMatches, value: textMatches,
    visible: flags, enabled: flags, hittable: flags, accessible: flags,
  }),
});
export interface IOSConfirmationProjection {
  format: typeof format;
  status: 'complete' | 'unavailable' | 'malformed' | 'truncated';
  reason: 'none' | 'non-string' | 'syntax' | 'unsupported' | 'input' | 'nodes' | 'depth' | 'bytes' | 'local-time';
  inspectedUnits: number;
  unknownTypes: number;
  invalidAttributes: number;
  nodes: string[];
}

export function projectIOSConfirmation(source: unknown, now: () => number = () => performance.now(),
  deadline = now() + IOS_CONFIRMATION_CAPTURE_LIMITS.localMs): IOSConfirmationProjection {
  const result: IOSConfirmationProjection = {
    format, status: 'complete', reason: 'none', inspectedUnits: 0, unknownTypes: 0, invalidAttributes: 0, nodes: [],
  };
  if (typeof source !== 'string') return { ...result, status: 'unavailable', reason: 'non-string' };
  const limit = Math.min(source.length, IOS_CONFIRMATION_CAPTURE_LIMITS.inputUnits);
  const stack: Array<{ tag: string; index: number }> = [];
  let position = 0;
  let roots = 0;
  const stop = (status: IOSConfirmationProjection['status'], reason: IOSConfirmationProjection['reason']): never => {
    result.status = status;
    result.reason = reason;
    throw result;
  };
  const checkTime = () => {
    if (now() >= deadline) stop('truncated', 'local-time');
  };
  const take = (): string => {
    if (position >= limit) stop(source.length > limit ? 'truncated' : 'malformed', source.length > limit ? 'input' : 'syntax');
    if (position % 256 === 0) checkTime();
    return source[position++];
  };
  const whitespace = () => { while (position < limit && /[\t\r\n ]/u.test(source[position])) take(); };
  const name = (): string => {
    const start = position;
    if (position >= limit || !/[A-Za-z_]/u.test(source[position])) stop('malformed', 'syntax');
    take();
    while (position < limit && /[A-Za-z0-9_.:-]/u.test(source[position])) take();
    return source.slice(start, position);
  };
  const match = (attributes: Map<string, string>, key: string): number => {
    const value = attributes.get(key);
    if (value === undefined) return 0;
    return value === 'Add' || value === 'Add to Home Screen' || value === 'Cancel' ? textMatches.indexOf(value) : 1;
  };
  const flag = (attributes: Map<string, string>, key: string): number => {
    const value = attributes.get(key);
    if (value === 'true') return 2;
    if (value === 'false') return 1;
    if (value !== undefined) result.invalidAttributes++;
    return 0;
  };
  const geometry = (attributes: Map<string, string>, key: string): number | null => {
    const value = attributes.get(key);
    if (value === undefined) return null;
    const number = value.length <= 32 && /^-?\d+(?:\.\d+)?$/u.test(value) ? Number(value) : NaN;
    if (Number.isFinite(number) && Math.abs(number) <= 1_000_000 && (!['width', 'height'].includes(key) || number >= 0)) return number;
    result.invalidAttributes++;
    return null;
  };
  try {
    checkTime();
    while (position < limit) {
      whitespace();
      if (position === limit) break;
      if (take() !== '<') stop('malformed', 'syntax');
      if (source[position] === '?') {
        if (roots || position !== 1) stop('malformed', 'unsupported');
        const start = position - 1;
        let end = take();
        while (end !== '>') end = take();
        if (!/^<\?xml version=(["'])1\.0\1(?: encoding=(["'])UTF-8\2)?(?: standalone=(["'])(?:yes|no)\3)?\?>$/u.test(source.slice(start, position))) stop('malformed', 'unsupported');
        continue;
      }
      if (source[position] === '!') stop('malformed', 'unsupported');
      if (source[position] === '/') {
        take();
        const tag = name();
        whitespace();
        if (take() !== '>' || stack.pop()?.tag !== tag) stop('malformed', 'syntax');
        continue;
      }
      const tag = name();
      const attributes = new Map<string, string>();
      let closed = false;
      while (position < limit) {
        const before = position;
        whitespace();
        if (source[position] === '/' || source[position] === '>') {
          closed = source[position] === '/';
          if (closed) take();
          if (take() !== '>') stop('malformed', 'syntax');
          break;
        }
        if (before === position) stop('malformed', 'syntax');
        const key = name();
        if (attributes.has(key)) stop('malformed', 'syntax');
        whitespace();
        if (take() !== '=') stop('malformed', 'syntax');
        whitespace();
        const quote = take();
        if (quote !== '"' && quote !== "'") stop('malformed', 'syntax');
        const start = position;
        let character = take();
        while (character !== quote) {
          if (character === '<' || (character.charCodeAt(0) < 32 && !/[\t\r\n]/u.test(character))) stop('malformed', 'syntax');
          if (character === '&') {
            let entity = '';
            for (let count = 0; count < 6; count++) {
              const next = take();
              entity += next;
              if (next === ';') break;
            }
            if (!['amp;', 'lt;', 'gt;', 'quot;', 'apos;'].includes(entity)) stop('malformed', 'unsupported');
          }
          character = take();
        }
        attributes.set(key, source.slice(start, position - 1));
      }
      if (position === limit && source[position - 1] !== '>') stop(source.length > limit ? 'truncated' : 'malformed', source.length > limit ? 'input' : 'syntax');
      if (!stack.length && ++roots > 1) stop('malformed', 'syntax');
      if (result.nodes.length >= IOS_CONFIRMATION_CAPTURE_LIMITS.nodes) stop('truncated', 'nodes');
      if (stack.length >= IOS_CONFIRMATION_CAPTURE_LIMITS.depth) stop('truncated', 'depth');
      const type = Math.max(0, nativeTypes.indexOf(tag));
      const node = JSON.stringify([
        stack.at(-1)?.index ?? null, stack.length, type,
        match(attributes, 'name'), match(attributes, 'label'), match(attributes, 'value'),
        flag(attributes, 'visible'), flag(attributes, 'enabled'), flag(attributes, 'hittable'), flag(attributes, 'accessible'),
        geometry(attributes, 'x'), geometry(attributes, 'y'), geometry(attributes, 'width'), geometry(attributes, 'height'),
      ]);
      const index = result.nodes.push(node) - 1;
      result.inspectedUnits = position;
      const persisted = JSON.stringify({ evidence: { confirmationCapture: { projection: result } } }, null, 2);
      // Reserve scalar counter/status growth and the terminal newline at the deepest evidence nesting.
      if (Buffer.byteLength(persisted) + 64 > IOS_CONFIRMATION_CAPTURE_LIMITS.projectionBytes) {
        result.nodes.pop();
        stop('truncated', 'bytes');
      }
      if (type === 0) result.unknownTypes++;
      if (!closed) stack.push({ tag, index });
      checkTime();
    }
    if (source.length > limit) stop('truncated', 'input');
    if (stack.length || roots !== 1) stop('malformed', 'syntax');
    checkTime();
  } catch (error) {
    if (error !== result) throw error;
  }
  result.inspectedUnits = position;
  return result;
}
