import assert from 'node:assert/strict';
import { IOS_CONFIRMATION_CAPTURE_LIMITS as limits, projectIOSConfirmation, type IOSConfirmationProjection } from '../support/ios-confirmation-capture';
import { sanitizeValue } from '../support/diagnostics';

type DecodedNode = {
  parent: number | null; depth: number; type: string; name: string; label: string; value: string;
  visible: boolean | 'unknown'; enabled: boolean | 'unknown'; hittable: boolean | 'unknown'; accessible: boolean | 'unknown';
  x: number | null; y: number | null; width: number | null; height: number | null;
};

export function decodeConfirmationProjection(projection: IOSConfirmationProjection): DecodedNode[] {
  assert.equal(projection.format.version, 2);
  assert.equal(projection.format.encoding, 'json-array-rows');
  return projection.nodes.map(row => {
    assert.match(row, /^[\x20-\x7e]+$/u);
    const values = JSON.parse(row) as Array<number | null>;
    assert.equal(values.length, projection.format.columns.length);
    return Object.fromEntries(projection.format.columns.map((column, index) => {
      const enums = projection.format.enums[column as keyof typeof projection.format.enums];
      const value = values[index];
      if (!enums) return [column, value];
      assert.ok(value !== null && Number.isInteger(value) && value >= 0 && value < enums.length);
      return [column, enums[value]];
    })) as DecodedNode;
  });
}

export function confirmationLateTargetFixture(): { source: string; nodes: DecodedNode[] } {
  const node = (fields: Partial<DecodedNode>): DecodedNode => ({
    parent: null, depth: 0, type: 'unknown', name: 'missing', label: 'missing', value: 'missing',
    visible: 'unknown', enabled: 'unknown', hittable: 'unknown', accessible: 'unknown',
    x: null, y: null, width: null, height: null, ...fields,
  });
  const prefix = Array.from({ length: 400 }, (_, index) => `<XCUIElementTypeOther name="unrelated-${index}" label="hidden content" value="https://private.test" visible="false" enabled="true" hittable="false" accessible="true" x="${index}" y="-1.25" width="20.5" height="0"/>`).join('');
  return {
    source: `<AppiumAUT><XCUIElementTypeWindow visible="false"><unrelated-wrapper visible="false">${prefix}</unrelated-wrapper><XCUIElementTypeOther visible="true"><XCUIElementTypeNavigationBar name="Add to Home Screen"><XCUIElementTypeButton name="Add" label="Cancel" value="Add to Home Screen" visible="true" enabled="true" hittable="true" accessible="false" x="-1000000" y="1000000" width="1000000" height="0"/><unknown-control name="Cancel" label="Add to Home Screen" value="Add"/><XCUIElementTypeStaticText name="Add to Home Screen" label="Add" value="Cancel"/><XCUIElementTypeButton name="missing" label="withheld" value="Add "/></XCUIElementTypeNavigationBar></XCUIElementTypeOther></XCUIElementTypeWindow></AppiumAUT>`,
    nodes: [
      node({}), node({ parent: 0, depth: 1, type: 'XCUIElementTypeWindow', visible: false }),
      node({ parent: 1, depth: 2, visible: false }),
      ...Array.from({ length: 400 }, (_, index) => node({
        parent: 2, depth: 3, type: 'XCUIElementTypeOther', name: 'withheld', label: 'withheld', value: 'withheld',
        visible: false, enabled: true, hittable: false, accessible: true, x: index, y: -1.25, width: 20.5, height: 0,
      })),
      node({ parent: 1, depth: 2, type: 'XCUIElementTypeOther', visible: true }),
      node({ parent: 403, depth: 3, type: 'XCUIElementTypeNavigationBar', name: 'Add to Home Screen' }),
      node({ parent: 404, depth: 4, type: 'XCUIElementTypeButton', name: 'Add', label: 'Cancel', value: 'Add to Home Screen',
        visible: true, enabled: true, hittable: true, accessible: false, x: -1_000_000, y: 1_000_000, width: 1_000_000, height: 0 }),
      node({ parent: 404, depth: 4, name: 'Cancel', label: 'Add to Home Screen', value: 'Add' }),
      node({ parent: 404, depth: 4, type: 'XCUIElementTypeStaticText', name: 'Add to Home Screen', label: 'Add', value: 'Cancel' }),
      node({ parent: 404, depth: 4, type: 'XCUIElementTypeButton', name: 'withheld', label: 'withheld', value: 'withheld' }),
    ],
  };
}

export function confirmationBytePressureFixture(): { source: string; coordinate: string } {
  const coordinate = '0.000000000001234567890123456';
  return { coordinate, source: `<A>${`<XCUIElementTypeOther x="-${coordinate}" y="${coordinate}" width="${coordinate}" height="${coordinate}"/>`.repeat(511)}</A>` };
}

export const iosConfirmationProjectionCases: Array<[string, () => Promise<void>]> = [
  ['confirmation capture projection preserves hidden parents unrelated nodes and unknown wrappers without readiness', async () => {
    const projection = projectIOSConfirmation('<AppiumAUT><XCUIElementTypeWindow visible="false"><private-wrapper><XCUIElementTypeNavigationBar name="Add to Home Screen"><XCUIElementTypeButton name="private-id" label="Add" value="Cancel" enabled="true" hittable="false" x="-1.5" y="0" width="20" height="10"/></XCUIElementTypeNavigationBar></private-wrapper><XCUIElementTypeButton label="Add"/></XCUIElementTypeWindow></AppiumAUT>');
    assert.equal(projection.status, 'complete');
    const nodes = decodeConfirmationProjection(projection);
    assert.deepEqual(nodes.map(node => node.parent), [null, 0, 1, 2, 3, 1]);
    assert.deepEqual(nodes.map(node => node.depth), [0, 1, 2, 3, 4, 2]);
    assert.equal(projection.unknownTypes, 2);
    assert.equal(nodes[1].visible, false);
    assert.equal(nodes[4].name, 'withheld');
    assert.equal(nodes[4].label, 'Add');
    assert.equal(nodes[4].value, 'Cancel');
    assert.equal(nodes[5].name, 'missing');
    assert.equal(nodes[4].enabled, true);
    assert.equal(nodes[4].hittable, false);
    assert.equal(nodes[4].visible, 'unknown');
    assert.equal(nodes[4].x, -1.5);
    assert.equal('ready' in projection, false);
    assert.doesNotMatch(JSON.stringify(projection), /private/u);
  }],
  ['confirmation capture projection retains unexpected native dialog types and bounded finite geometry', async () => {
    const types = ['Alert', 'Sheet', 'Toolbar', 'TextField', 'SecureTextField', 'SearchField', 'TextView'];
    const projection = projectIOSConfirmation(`<XCUIElementTypeWindow>${types.map(type => `<XCUIElementType${type} visible="yes" accessible="secret" x="Infinity" y="1000001" width="-1" height="NaN"/>`).join('')}</XCUIElementTypeWindow>`);
    assert.equal(projection.status, 'complete');
    assert.equal(projection.invalidAttributes, types.length * 6);
    const nodes = decodeConfirmationProjection(projection);
    assert.deepEqual(nodes.slice(1).map(node => node.type), types.map(type => `XCUIElementType${type}`));
    assert.ok(nodes.every(node => [node.x, node.y, node.width, node.height].every(value => value === null)));
    assert.doesNotMatch(JSON.stringify(projection), /secret|Infinity|NaN/u);
  }],
  ['confirmation capture projection quote-aware hostile multibyte labels and URLs are withheld', async () => {
    const secret = '秘密🔑https://private.test/?token=private&amp;next=secret > <not-a-node';
    const projection = projectIOSConfirmation(`<?xml version="1.0" encoding="UTF-8"?><XCUIElementTypeButton name="${secret.replace('<', '&lt;')}" label='Cancel > private' value="Add to Home Screen suffix" identifier="private"/>`);
    assert.equal(projection.status, 'complete');
    assert.equal(projection.nodes.length, 1);
    const [node] = decodeConfirmationProjection(projection);
    assert.deepEqual([node.name, node.label, node.value], ['withheld', 'withheld', 'withheld']);
    assert.doesNotMatch(JSON.stringify(projection), /秘密|🔑|private|https|suffix/u);
  }],
  ['confirmation capture projection rejects malformed XML declarations and unsupported entities without fragments', async () => {
    for (const source of ['', '<A>', '<A></B>', '<A/><B/>', '<A name="private/>', '<A name="a" name="b"/>', '<A>private</A>', '<!DOCTYPE A [<!ENTITY secret SYSTEM "https://private.test">]><A/>', '<A name="&secret;"/>', '<A name="&#65;dd"/>', '<?private secret?><A/>', '<A name="<private"/>', '<A / >']) {
      const projection = projectIOSConfirmation(source);
      assert.equal(projection.status, 'malformed', source);
      assert.doesNotMatch(JSON.stringify(projection), /private|secret|DOCTYPE|https/u);
    }
    assert.equal(projectIOSConfirmation(null).status, 'unavailable');
    assert.equal(projectIOSConfirmation({ source: 'secret' }).reason, 'non-string');
  }],
  ['confirmation capture compact rows preserve the complete long hidden prefix late targets and every decoded field', async () => {
    const fixture = confirmationLateTargetFixture();
    const projection = projectIOSConfirmation(fixture.source);
    assert.equal(projection.status, 'complete');
    assert.equal(projection.reason, 'none');
    assert.equal(projection.inspectedUnits, fixture.source.length);
    assert.equal(projection.unknownTypes, 3);
    assert.equal(projection.invalidAttributes, 0);
    assert.deepEqual(decodeConfirmationProjection(projection), fixture.nodes);
    assert.deepEqual(projection.format.columns, ['parent', 'depth', 'type', 'name', 'label', 'value', 'visible', 'enabled', 'hittable', 'accessible', 'x', 'y', 'width', 'height']);
    const persisted = `${JSON.stringify(sanitizeValue({ evidence: { confirmationCapture: { projection } } }), null, 2)}\n`;
    assert.doesNotMatch(persisted, /unrelated|hidden content|private|https|unknown-control/u);
    assert.ok(Buffer.byteLength(persisted) < limits.projectionBytes);
    assert.deepEqual(decodeConfirmationProjection(JSON.parse(persisted).evidence.confirmationCapture.projection), fixture.nodes);
  }],
  ['confirmation capture compact rows enforce actual nested pretty printed bytes without rounding finite geometry', async () => {
    const { source, coordinate } = confirmationBytePressureFixture();
    assert.ok(source.length < limits.inputUnits);
    const projection = projectIOSConfirmation(source);
    assert.equal(projection.status, 'truncated');
    assert.equal(projection.reason, 'bytes');
    assert.ok(projection.nodes.length > 300 && projection.nodes.length < limits.nodes);
    const nodes = decodeConfirmationProjection(projection);
    assert.ok(nodes.slice(1).every(node => node.parent === 0 && node.depth === 1 && node.x === -Number(coordinate)
      && node.y === Number(coordinate) && node.width === Number(coordinate) && node.height === Number(coordinate)));
    const persisted = `${JSON.stringify(sanitizeValue({ evidence: { confirmationCapture: { projection } } }), null, 2)}\n`;
    assert.ok(Buffer.byteLength(persisted) <= limits.projectionBytes);
    assert.ok(Buffer.byteLength(persisted) > limits.projectionBytes - 256);
    assert.deepEqual(decodeConfirmationProjection(JSON.parse(persisted).evidence.confirmationCapture.projection), nodes);
  }],
  ['confirmation capture compact rows stop at the local cutoff after retaining an ordered prefix', async () => {
    const fixture = confirmationLateTargetFixture();
    let calls = 0;
    const projection = projectIOSConfirmation(fixture.source, () => ++calls < 100 ? 999 : 1_000, 1_000);
    assert.equal(projection.status, 'truncated');
    assert.equal(projection.reason, 'local-time');
    assert.equal(calls, 100);
    assert.ok(projection.inspectedUnits < fixture.source.length);
    assert.ok(projection.nodes.length > 0 && projection.nodes.length < fixture.nodes.length);
    assert.deepEqual(decodeConfirmationProjection(projection), fixture.nodes.slice(0, projection.nodes.length));
  }],
  ['confirmation capture projection enforces input node depth byte and local work bounds', async () => {
    const input = projectIOSConfirmation(`<A name="${'秘密'.repeat(limits.inputUnits)}"/>`);
    assert.equal(input.status, 'truncated');
    assert.equal(input.reason, 'input');
    assert.equal(input.inspectedUnits, limits.inputUnits);
    const nodes = projectIOSConfirmation(`<A>${'<XCUIElementTypeButton label="Add"/>'.repeat(600)}</A>`);
    assert.equal(nodes.status, 'truncated');
    assert.equal(nodes.reason, 'nodes');
    assert.equal(nodes.nodes.length, limits.nodes);
    assert.ok(Buffer.byteLength(JSON.stringify(nodes, null, 2)) <= limits.projectionBytes);
    assert.ok(decodeConfirmationProjection(nodes).every((node, index) => node.parent === null || node.parent < index));
    const depth = projectIOSConfirmation('<A>'.repeat(65) + '</A>'.repeat(65));
    assert.equal(depth.status, 'truncated');
    assert.equal(depth.reason, 'depth');
    assert.equal(depth.nodes.length, limits.depth);
    let clock = 0;
    const timed = projectIOSConfirmation('<A/>', () => clock += 1_000, 1_000);
    assert.equal(timed.status, 'truncated');
    assert.equal(timed.reason, 'local-time');
    assert.deepEqual(timed.nodes, []);
  }],
];
