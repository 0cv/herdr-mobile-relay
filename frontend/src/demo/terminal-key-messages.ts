import type { CommandResult } from '../lib/types';

export function keyResultMessage(result: CommandResult): string {
  const message = result.data?.message;
  return typeof message === 'string' ? message : '';
}
