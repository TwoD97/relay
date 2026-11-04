import { useEffect, useRef, useState } from "react";
import { Columns2, Plus, Search, X } from "lucide-react";
import { Modal } from "./Modal";
import { SessionView } from "./Views";
import { fleetWithoutMaintenance } from "./MaintenanceJobs";
import { canCreateSessions } from "./runtimeCapabilities";
import { MAX_TERMINAL_PANES, paneID, terminalPanes } from "./terminalLayout";
import type { FleetState, Host, Selection, Session, TerminalPaneRef } from "./types";
import "./TerminalWorkspace.css";

type Props = {
  state: FleetState; selection: Selection;
  onLayout: (panes: TerminalPaneRef[], active: number, expanded: boolean, replace?: boolean) => void;
  onActivity: () => void; onHost: (host: Host) => void; onEmpty: (hostId: string) => void;
  onNew: (host: Host) => void; onDelete: (host: Host, session: Session) => void;
  onReviewed: (host: Host, session: Session) => void;
  onCreated: (host: Host, source: Session, next: Session) => void;
  onRenamed: (host: Host, session: Session) => void;
};

export function TerminalWorkspace({ state, selection, onLayout, onActivity, onHost, onEmpty, onNew, onDelete, onReviewed, onCreated, onRenamed }: Props) {
  const panes = terminalPanes(selection);
  const [picker, setPicker] = useState<"add" | number | null>(null);
  const [query, setQuery] = useState("");
  const grid = useRef<HTMLDivElement>(null);
  const pickedPane = useRef<TerminalPaneRef | null>(null);
  const identities = useRef(new Map<string, string>());
  useEffect(() => {
    const target = pickedPane.current;
    if (picker !== null || !target) return;
    pickedPane.current = null;
    const node = [...(grid.current?.querySelectorAll<HTMLElement>(".terminal-pane") ?? [])].find((pane) => pane.dataset.paneHost === target.host && pane.dataset.paneSession === target.session);
    // Focus the selected pane's title, never its input: opening a viewer does
    // not itself take control or send anything to the remote process.
    (node?.querySelector<HTMLElement>(".session-title-button") ?? node)?.focus({ preventScroll: true });
  }, [picker]);
  // A refreshed list must never silently bind a pane to a replacement process.
  const pinned = panes.map((pane) => {
    const id = paneID(pane);
    const current = state.runtimes[pane.host]?.sessions.find((item) => item.id === pane.session);
    if (!identities.current.has(id) && (pane.createdAt || current)) identities.current.set(id, pane.createdAt ?? current!.createdAt);
    return { ...pane, createdAt: pane.createdAt ?? identities.current.get(id) };
  });
  const active = Math.max(0, pinned.findIndex((pane) => pane.host === selection?.host && pane.session === selection.session));
  const expanded = Boolean(selection?.expanded);
  const openPicker = (target: "add" | number) => { pickedPane.current = null; setQuery(""); setPicker(target); };
  const choose = (host: Host, session: Session) => {
    if (picker === null || (picker === "add" && pinned.length >= MAX_TERMINAL_PANES)) return;
    const next = { host: host.id, session: session.id, createdAt: session.createdAt };
    if (pinned.some((pane, index) => index !== picker && paneID(pane) === paneID(next))) return;
    identities.current.set(paneID(next), next.createdAt);
    pickedPane.current = next;
    const items = picker === "add" ? [...pinned, next] : pinned.map((pane, index) => index === picker ? next : pane);
    onLayout(items, picker === "add" ? items.length - 1 : picker, false);
    setPicker(null);
  };
  const close = (index: number) => {
    const items = pinned.filter((_, current) => current !== index);
    if (!items.length) onEmpty(pinned[index].host);
    else onLayout(items, index === active ? Math.min(index, items.length - 1) : active - (index < active ? 1 : 0), expanded);
  };
  const pickerState = fleetWithoutMaintenance(state);
  const choices = pickerState.hosts.flatMap((host) => (pickerState.runtimes[host.id]?.sessions ?? [])
    .filter((session) => !pinned.some((pane, index) => index !== picker && pane.host === host.id && pane.session === session.id))
    .filter((session) => `${host.name} ${session.title} ${session.workspace} ${session.cwd}`.toLowerCase().includes(query.trim().toLowerCase()))
    .map((session) => ({ host, session })));
  return <>
    <div ref={grid} className={`terminal-workspace ${expanded ? "terminal-workspace-zoomed" : ""}`} data-pane-count={panes.length} aria-label="Terminal workspace">
      {pinned.map((pane, index) => {
        const host = state.hosts.find((item) => item.id === pane.host);
        const runtime = state.runtimes[pane.host];
        const candidate = runtime?.sessions.find((item) => item.id === pane.session);
        const session = candidate?.createdAt === pane.createdAt ? candidate : undefined;
        const loading = state.loading || Boolean(host && !runtime?.fetchedAt && !runtime?.sessionsError && !runtime?.error);
        const hidden = expanded && index !== active;
        const focusPane = () => { if (active !== index) onLayout(pinned, index, expanded, true); };
        const actions = <><button className="icon-button" aria-label="Add terminal pane" title={pinned.length === MAX_TERMINAL_PANES ? "Four panes are already open" : "Add terminal pane"} disabled={pinned.length >= MAX_TERMINAL_PANES} onClick={() => openPicker("add")}><Columns2 size={16} /></button><button className="icon-button close-terminal-pane" aria-label="Close pane" title="Close this pane; the session keeps running" onClick={() => close(index)}><X size={16} /></button></>;
        return <section key={paneID(pane)} className="terminal-pane" role="region" tabIndex={-1} aria-label={`${session?.title ?? "Unavailable session"} on ${host?.name ?? "Unavailable machine"} pane`} data-pane-host={pane.host} data-pane-session={pane.session} data-active={index === active} hidden={hidden} inert={hidden} onPointerDownCapture={focusPane} onFocusCapture={focusPane}>
          {host && session ? <SessionView key={session.createdAt} host={host} session={session} runtime={runtime} expanded={expanded} onExpand={() => onLayout(pinned, index, !expanded)} paneActions={actions} onActivity={onActivity} onDelete={() => onDelete(host, session)} onHost={() => onHost(host)} onReviewed={() => onReviewed(host, session)} onCreated={(next) => onCreated(host, session, next)} onRenamed={(next) => onRenamed(host, next)} /> : <div className="terminal-pane-unavailable"><div className="terminal-pane-unavailable-actions">{actions}</div><h2>{loading ? "Loading session…" : !host ? "This machine is unavailable." : "This session is unavailable."}</h2><p>{candidate ? "The session identity has changed. Choose a session to continue." : runtime?.sessionsError || runtime?.error || "Choose another session, or open its machine to check the connection."}</p><button className="button secondary" onClick={() => openPicker(index)}>Choose a session</button>{host && <button className="text-button" onClick={() => onHost(host)}>Open machine</button>}</div>}
        </section>;
      })}
    </div>
    {picker !== null && <Modal title={picker === "add" ? "Add terminal pane" : "Choose a session"} subtitle="View up to four sessions from any of your machines." restoreFocus={() => pickedPane.current === null} onClose={() => setPicker(null)}>
      <div className="modal-body terminal-picker"><label className="terminal-picker-search"><Search size={15} /><input data-autofocus="true" aria-label="Find a session" placeholder="Search sessions or machines…" value={query} onChange={(event) => setQuery(event.target.value)} /></label>
        <div className="terminal-picker-list">{choices.map(({ host, session }) => <button key={JSON.stringify([host.id, session.id, session.createdAt])} className="terminal-picker-session" aria-label={`${session.title} on ${host.name}`} onClick={() => choose(host, session)}><strong>{session.title}</strong><span>{host.name} · {session.workspace || session.cwd} · {session.status}</span></button>)}{!choices.length && <p className="muted">{query ? "No matching sessions." : "All available sessions are already in this view."}</p>}</div>
        {picker === "add" && pinned.length < MAX_TERMINAL_PANES && <div className="terminal-picker-new">{state.hosts.filter((host) => canCreateSessions(host, state.runtimes[host.id])).map((host) => <button key={host.id} className="button secondary compact" aria-label={`New session on ${host.name}`} onClick={() => { setPicker(null); onNew(host); }}><Plus size={14} />New session on {host.name}</button>)}</div>}
      </div>
    </Modal>}
  </>;
}
