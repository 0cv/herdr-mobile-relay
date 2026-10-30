import { sha256 } from '@noble/hashes/sha2.js';
import { bytesToHex } from '@noble/hashes/utils.js';

/** Incremental SHA-256 keeps whole files out of JS memory while still hashing the final body. */
export class IncrementalSha256 {
  private readonly hash = sha256.create();
  private finished = false;

  update(input: Uint8Array): void {
    if (this.finished) throw new Error('sha256_already_finished');
    this.hash.update(input);
  }

  digestHex(): string {
    if (this.finished) throw new Error('sha256_already_finished');
    this.finished = true;
    return bytesToHex(this.hash.digest());
  }
}
