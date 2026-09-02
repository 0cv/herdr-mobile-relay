interface HashRequest {
  id: string;
  file: File;
}

function hex(bytes: Uint8Array): string {
  let output = '';
  for (const byte of bytes) output += byte.toString(16).padStart(2, '0');
  return output;
}

self.addEventListener('message', async (event: MessageEvent<HashRequest>) => {
  const { id, file } = event.data;
  try {
    const digest = await crypto.subtle.digest('SHA-256', await file.arrayBuffer());
    self.postMessage({ id, digest: hex(new Uint8Array(digest)) });
  } catch {
    self.postMessage({ id, error: 'attachment_hash_failed' });
  }
});
