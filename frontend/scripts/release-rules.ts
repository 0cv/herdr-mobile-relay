export const appScriptPattern = /^app-[a-f0-9]{64}\.js$/;
export const appStylePattern = /^app-[a-f0-9]{64}\.css$/;
export const appScriptPathPattern = new RegExp(`^assets/${appScriptPattern.source.slice(1)}`);
export const appStylePathPattern = new RegExp(`^assets/${appStylePattern.source.slice(1)}`);
export const lazyScriptPattern = /^[A-Za-z0-9_.-]+-[0-9]+\.js$/;

export function productVersionFromManifest(manifest: string): string {
  const version = manifest.match(/^version = "([0-9]+\.[0-9]+\.[0-9]+)"$/m)?.[1];
  if (!version) throw new Error('herdr-plugin.toml must declare a MAJOR.MINOR.PATCH version');
  return version;
}

export function releaseEntryPath(descriptor: { files?: { entry?: { path?: unknown } } }): string {
  const entry = descriptor?.files?.entry?.path;
  if (typeof entry !== 'string' || !entry.startsWith('builds/') || !entry.endsWith('/index.html')) {
    throw new Error('release.json does not describe a build-specific entry');
  }
  return entry;
}
