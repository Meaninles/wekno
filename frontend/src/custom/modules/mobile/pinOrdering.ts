export function sortPinnedFirstByRecency<T>(
  items: readonly T[],
  pinnedKeys: readonly string[],
  keyOf: (item: T) => string,
): T[] {
  const itemByKey = new Map<string, T>();
  for (const item of items) {
    const key = keyOf(item);
    if (!key || itemByKey.has(key)) continue;
    itemByKey.set(key, item);
  }

  const pinnedKeySet = new Set(pinnedKeys);
  const pinnedItems = pinnedKeys
    .slice()
    .reverse()
    .map((key) => itemByKey.get(key))
    .filter((item): item is T => !!item);
  const unpinnedItems = items.filter((item) => !pinnedKeySet.has(keyOf(item)));

  return [...pinnedItems, ...unpinnedItems];
}
