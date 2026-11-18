import { describe, expect, it } from 'vitest';
import {
  appScriptPattern,
  appStylePattern,
  appScriptPathPattern,
  appStylePathPattern,
  lazyScriptPattern,
  productVersionFromManifest,
  releaseEntryPath,
} from '../../scripts/release-rules';

describe('release naming rules', () => {
  it('reads an exact product version from the plugin manifest', () => {
    expect(productVersionFromManifest('name = "relay"\nversion = "1.2.3"\n')).toBe('1.2.3');
  });

  it.each(['', 'version = "1.2"', 'version = "1.2.3-beta"', 'other_version = "1.2.3"'])(
    'rejects an invalid product version in %j', (manifest) => {
      expect(() => productVersionFromManifest(manifest)).toThrow('herdr-plugin.toml must declare a MAJOR.MINOR.PATCH version');
    },
  );

  it.each([
    [appScriptPattern, `app-${'a'.repeat(64)}.js`, `app-${'a'.repeat(63)}.js`],
    [appStylePattern, `app-${'0'.repeat(64)}.css`, `app-${'A'.repeat(64)}.css`],
    [appScriptPathPattern, `assets/app-${'a'.repeat(64)}.js`, `other/app-${'a'.repeat(64)}.js`],
    [appStylePathPattern, `assets/app-${'0'.repeat(64)}.css`, `app-${'0'.repeat(64)}.css`],
    [lazyScriptPattern, 'lazy_part.foo-007.js', 'lazy-part-hash.js'],
  ])('matches only the expected asset names with %s', (pattern, valid, invalid) => {
    expect(pattern.test(valid)).toBe(true);
    expect(pattern.test(invalid)).toBe(false);
    expect(pattern.test(`${valid}.extra`)).toBe(false);
    expect(pattern.test(`prefix/${valid}`)).toBe(false);
  });

  it('reads a build-specific entry from the release descriptor', () => {
    const path = 'builds/1.2.3-400-0123456789abcdef/index.html';
    expect(releaseEntryPath({ files: { entry: { path } } })).toBe(path);
  });

  it.each([undefined, 42, 'index.html', '/builds/example/index.html', 'builds/example/app.html'])(
    'rejects a non-build-specific entry %j', (path) => {
      expect(() => releaseEntryPath({ files: { entry: { path } } })).toThrow('release.json does not describe a build-specific entry');
    },
  );

  it('rejects a missing entry descriptor', () => {
    expect(() => releaseEntryPath({})).toThrow('release.json does not describe a build-specific entry');
  });
});
