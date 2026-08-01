export function getInitialWikiDisclosureExpanded(wikiEnabled: boolean): boolean {
  return wikiEnabled
}

export function syncWikiDisclosureExpanded(
  expanded: boolean,
  wikiEnabled: boolean,
): boolean {
  return wikiEnabled || expanded
}
