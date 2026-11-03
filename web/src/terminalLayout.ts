import type { Selection, TerminalPaneRef } from "./types";

export const MAX_TERMINAL_PANES = 4;
export const paneID = (pane: TerminalPaneRef) => JSON.stringify([pane.host, pane.session]);
export function terminalPanes(selection: Selection): TerminalPaneRef[] {
  if (!selection?.host || !selection.session) return [];
  return selection.panes?.length ? selection.panes : [{ host: selection.host, session: selection.session }];
}
export function paneSelection(panes: TerminalPaneRef[], active: number, expanded = false): Selection {
  const selected = panes[active];
  return selected ? { host: selected.host, session: selected.session, panes, expanded } : null;
}
export function terminalLayoutHash(selection: Selection): string | null {
  const panes = terminalPanes(selection);
  if (panes.length < 2) return null;
  const active = Math.max(0, panes.findIndex((pane) => pane.host === selection?.host && pane.session === selection.session));
  const query = new URLSearchParams({ panes: JSON.stringify(panes), active: String(active) });
  if (selection?.expanded) query.set("expanded", "1");
  return `terminals?${query}`;
}
export function readTerminalLayout(hash: string): Selection {
  if (hash.length > 4096) return null;
  const query = new URLSearchParams(hash.slice("terminals?".length));
  try {
    const panes: unknown = JSON.parse(query.get("panes") ?? "null");
    const field = (value: unknown) => typeof value === "string" && value.length > 0 && value.length <= 256 && !/[\u0000-\u001f]/.test(value);
    if (!Array.isArray(panes) || panes.length < 1 || panes.length > MAX_TERMINAL_PANES) return null;
    const clean: TerminalPaneRef[] = [];
    for (const item of panes) {
      if (!item || typeof item !== "object" || !field(item.host) || !field(item.session) || (item.createdAt !== undefined && !field(item.createdAt))) return null;
      clean.push({ host: item.host, session: item.session, ...(item.createdAt ? { createdAt: item.createdAt } : {}) });
    }
    if (new Set(clean.map(paneID)).size !== clean.length) return null;
    const active = Number(query.get("active") ?? "0");
    if (!Number.isInteger(active) || active < 0 || active >= clean.length) return null;
    return paneSelection(clean, active, query.get("expanded") === "1");
  } catch { return null; }
}
