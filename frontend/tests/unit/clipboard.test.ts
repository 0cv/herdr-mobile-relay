import { afterEach, expect, it, vi } from 'vitest';
import { copyText } from '$lib/clipboard';

afterEach(() => vi.unstubAllGlobals());

it('copies text to the clipboard', async () => {
  const writeText = vi.fn().mockResolvedValue(undefined);
  vi.stubGlobal('navigator', { clipboard: { writeText } });

  expect(await copyText('Agent response')).toBe(true);
  expect(writeText).toHaveBeenCalledWith('Agent response');
});

it('returns false when clipboard access is unavailable', async () => {
  vi.stubGlobal('navigator', {});

  expect(await copyText('Agent response')).toBe(false);
});

it('returns false when clipboard access is refused', async () => {
  const writeText = vi.fn().mockRejectedValue(new Error('Permission denied'));
  vi.stubGlobal('navigator', { clipboard: { writeText } });

  expect(await copyText('Agent response')).toBe(false);
});
