import { createServer } from 'node:net';
import { lstat, mkdtemp, rm } from 'node:fs/promises';
import { join, relative, sep } from 'node:path';
import { tmpdir } from 'node:os';

export async function herdrSocketFixture() {
  const reserved = process.env.HERDR_TEST_PI_SOCKET;
  if (reserved) {
    const stat = await lstat(reserved);
    const path = relative(tmpdir(), reserved);
    if (!stat.isSocket() || stat.uid !== process.getuid() || path === '..' || path.startsWith(`..${sep}`)) throw new Error('Preallocated Pi fixture socket must be owned and under the selected temporary directory');
    return { path: reserved, close: async () => {} };
  }
  const directory = await mkdtemp(join(tmpdir(), 'pi-'));
  const path = join(directory, 'herdr.sock');
  const server = createServer();
  try {
    await new Promise((resolve, reject) => { server.once('error', reject); server.listen(path, resolve); });
  } catch (error) {
    await rm(directory, { recursive: true, force: true });
    throw error;
  }
  return { path, close: async () => {
    await new Promise((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
    await rm(directory, { recursive: true, force: true });
  } };
}
