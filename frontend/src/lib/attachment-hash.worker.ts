import { bytesToHex } from '@noble/hashes/utils.js';
interface HashRequest {
  id: string;
  file: File;
}

self.addEventListener('message', async (event: MessageEvent<HashRequest>) => {
  const { id, file } = event.data;
  try {
    const digest = await crypto.subtle.digest('SHA-256', await file.arrayBuffer());
    self.postMessage({ id, digest: bytesToHex(new Uint8Array(digest)) });
  } catch {
    self.postMessage({ id, error: 'attachment_hash_failed' });
  }
});
