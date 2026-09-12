type ClearConversationPreviews = (relayId?: string) => void;
const clearers = new Set<ClearConversationPreviews>();

export function registerConversationCacheClearer(clearer: ClearConversationPreviews): void {
  clearers.add(clearer);
}

export function clearConversationPreviews(): void {
  for (const clearer of clearers) clearer();
}

export function clearConversationPreviewsForRelay(relayId: string): void {
  if (!relayId) return;
  for (const clearer of clearers) clearer(relayId);
}
