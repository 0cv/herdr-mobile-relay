export function parseAndroidAvdName(response: string): string | undefined {
  for (const line of response.split(/\r?\n/u).map((value) => value.trim())) {
    if (!line || line === 'OK' || /^KO(?:\s|:|$)/u.test(line)) continue;
    return line.split(/\s+/u)[0];
  }
  return undefined;
}
