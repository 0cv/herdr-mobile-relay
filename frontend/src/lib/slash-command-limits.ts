import limits from '../../../contracts/fixtures/slash_command_limits.json';

// Keep the phone's validation cap in the shared wire-contract fixture with the
// relay's catalog cap. The relay still owns the filesystem budget.
export const SLASH_COMMAND_MAX_ENTRIES = limits.max_entries;
