export const maximumBounds = { workMs: 420000, cleanupMs: 45000, workBytes: 1048576, cleanupBytes: 262144 };

export function validateBounds(bounds) {
  if (!bounds || Object.keys(bounds).sort().join() !== Object.keys(maximumBounds).sort().join() || Object.entries(maximumBounds).some(([key, max]) => !Number.isSafeInteger(bounds[key]) || bounds[key] < 1 || bounds[key] > max)) throw new Error("invalid qualification bounds");
  return bounds;
}
