import { useEffect, useMemo, useState } from "react";
import { ArrowUpRight, Bell, ChevronDown, ChevronRight, Folder, Layers3, Plus, Search, Server, Terminal, X } from "lucide-react";
import type { FleetState, Host, Selection, Session } from "./types";
import { maintenanceNeedsAttention } from "./MaintenanceJobs";

export function HostStatus({ host, compact = false }: { host: Host; compact?: boolean }) {
  return <span className={`status-pill status-${host.status}`}><span className="status-dot" />{compact ? host.status === "online" ? "Online" : host.status === "error" ? "Needs attention" : host.status === "disconnected" ? "Offline" : "Connecting" : host.status[0].toUpperCase() + host.status.slice(1)}</span>;
}

export function SessionStatus({ session }: { session: Session }) {
  const maintenance = session.purpose === "install" || session.purpose === "update" || session.purpose === "repair";
  const succeeded = maintenance && session.status === "exited" && session.exitCode === 0;
  const completed = session.purpose === "update" ? "Updated" : session.purpose === "repair" ? "Repaired" : "Installed";
  const running = session.purpose === "update" ? "Updating" : session.purpose === "repair" ? "Repairing" : session.purpose === "install" ? "Installing" : "Running";
  return <span className={`session-status session-${succeeded ? "installed" : session.status}`}><span className="status-dot" />{succeeded ? completed : session.status === "running" ? running : session.status === "interrupted" ? "Interrupted" : session.exitCode == null ? "Exited" : `Exited · ${session.exitCode}`}</span>;
}

function readCollapsed(): Record<string, boolean> {
  try { const value: unknown = JSON.parse(localStorage.getItem("relay-collapsed") ?? "{}"); return value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, boolean> : {}; } catch { return {}; }
}

export function Sidebar({ state, selected, mobileOpen, onClose, onSelect, onAdd }: { state: FleetState; selected: Selection; mobileOpen: boolean; onClose: () => void; onSelect: (selection: Selection) => void; onAdd: () => void }) {
  const [query, setQuery] = useState("");
  const [collapsed, setCollapsed] = useState(readCollapsed);
  const [mobile, setMobile] = useState(() => window.matchMedia("(max-width: 800px)").matches);
  useEffect(() => { const query = window.matchMedia("(max-width: 800px)"); const update = () => setMobile(query.matches); query.addEventListener("change", update); return () => query.removeEventListener("change", update); }, []);
  const toggle = (key: string) => setCollapsed((old) => { const next = { ...old, [key]: !old[key] }; try { localStorage.setItem("relay-collapsed", JSON.stringify(next)); } catch { /* The tree remains usable without storage. */ } return next; });
  const entries = useMemo(() => {
    const match = query.trim().toLowerCase();
    return state.hosts.map((host) => {
      const all = state.runtimes[host.id]?.sessions ?? [];
      const hostMatch = `${host.name} ${host.target}`.toLowerCase().includes(match);
      const sessions = all.filter((session) => !match || hostMatch || `${session.title} ${session.cwd} ${session.workspace} ${session.harness}`.toLowerCase().includes(match));
      const groups = new Map<string, Session[]>();
      for (const session of sessions) { const group = session.workspace || "Workspace"; groups.set(group, [...(groups.get(group) ?? []), session]); }
      return { host, groups: [...groups].sort(([a], [b]) => a.localeCompare(b)), visible: !match || hostMatch || sessions.length > 0 };
    }).filter((entry) => entry.visible);
  }, [query, state.hosts, state.runtimes]);
  const select = (value: Selection) => { onSelect(value); onClose(); };
  const attention = state.hosts.flatMap((host) => (state.runtimes[host.id]?.sessions ?? []).filter((session) => session.attention && session.attention.kind !== "completed").map((session) => ({ host, session })));
  const maintenanceAttention = new Set((state.maintenanceJobs ?? []).filter(maintenanceNeedsAttention).map((job) => job.hostId));
  const hostAttention = state.hosts.filter((host) => host.status === "error" || host.runtimeOperation?.status === "error" || host.setupWarning || maintenanceAttention.has(host.id));
  const attentionCount = attention.length + hostAttention.length;
  return <>
    {mobileOpen && <button className="sidebar-scrim" aria-label="Close navigation" onClick={onClose} />}
    <aside className={`sidebar ${mobileOpen ? "sidebar-open" : ""}`} aria-label="Fleet navigation" inert={mobile && !mobileOpen} aria-hidden={mobile && !mobileOpen ? true : undefined}>
      <div className="brand-row"><button className="brand" onClick={() => select(null)} aria-label="Relay home"><svg className="brand-mark" aria-hidden="true" viewBox="0 0 28 28"><path d="M7 21 21 7" /><circle cx="7" cy="21" r="4" /><circle cx="21" cy="7" r="4" /></svg><span>relay<span className="brand-period">.</span></span></button><button className="icon-button mobile-close" aria-label="Close navigation" onClick={onClose}><X size={18} /></button></div>
      <div className="sidebar-main"><button className={`fleet-nav ${!selected ? "active" : ""}`} onClick={() => select(null)}><Layers3 size={17} /><span>Fleet overview</span><span className="nav-count">{state.hosts.length}</span></button>
        <label className="sidebar-search"><Search size={15} /><input aria-label="Filter machines and sessions" placeholder="Find a machine or session…" value={query} onChange={(event) => setQuery(event.target.value)} />{query && <button aria-label="Clear filter" onClick={() => setQuery("")}><X size={13} /></button>}</label>
        {attentionCount > 0 && <section className="attention-pins" aria-label="Needs attention"><div className="attention-label"><Bell size={12} /><span>NEEDS ATTENTION</span><b>{attentionCount}</b></div>{hostAttention.map((host) => <button key={host.id} className="attention-pin" onClick={() => select({ host: host.id })}><strong>{host.name}</strong><span>{host.status === "error" ? "Connection needs attention" : host.runtimeOperation?.status === "error" ? "Runtime maintenance failed" : maintenanceAttention.has(host.id) ? "Agent maintenance needs attention" : "Agent coordination setup"}</span><ArrowUpRight size={13} /></button>)}{attention.map(({ host, session }) => <button key={`${host.id}/${session.id}`} className="attention-pin" onClick={() => select({ host: host.id, session: session.id })}><strong>{session.title}</strong><span>{host.name} · {session.attention?.kind === "permission" ? "Permission event" : "Notification"}</span><ArrowUpRight size={13} /></button>)}</section>}
        <div className="sidebar-section-label"><span>MACHINES</span><button aria-label="Add machine" title="Add machine" onClick={onAdd}><Plus size={15} /></button></div>
        <nav className="host-tree" aria-label="Machines and workspaces">{entries.map(({ host, groups }) => <div className="tree-host" key={host.id}>
          <div className={`tree-host-row ${selected?.host === host.id && !selected.session ? "selected" : ""}`}>
            <button className="tree-chevron" aria-label={`${collapsed[host.id] ? "Expand" : "Collapse"} ${host.name}`} aria-expanded={!collapsed[host.id]} onClick={() => toggle(host.id)}>{collapsed[host.id] && !query ? <ChevronRight size={14} /> : <ChevronDown size={14} />}</button>
            <button className="host-select" onClick={() => select({ host: host.id })}><Server size={16} /><span>{host.name}</span><span className={`host-dot status-${host.status}`} title={host.status} aria-label={host.status} /></button>
          </div>
          {(!collapsed[host.id] || query) && <div className={`tree-workspaces ${host.status !== "online" ? "tree-stale" : ""}`}>
            {groups.map(([workspace, sessions]) => {
              const key = JSON.stringify([host.id, workspace]);
              return <div className="tree-workspace" key={key}><button className="workspace-row" onClick={() => toggle(key)} aria-expanded={!collapsed[key]}>{collapsed[key] && !query ? <ChevronRight size={12} /> : <ChevronDown size={12} />}<Folder size={13} /><span>{workspace}</span><small>{sessions.length}</small></button>{(!collapsed[key] || query) && <div className="tree-sessions">{sessions.map((session) => <button key={session.id} className={`session-row ${selected?.host === host.id && selected.session === session.id ? "selected" : ""}`} onClick={() => select({ host: host.id, session: session.id })} title={session.cwd}><Terminal size={13} /><span>{session.title}</span><span className={`session-tree-dot session-${session.status}`} aria-label={session.status} /></button>)}</div>}</div>;
            })}
            {groups.length === 0 && <button className="tree-empty" onClick={() => select({ host: host.id })}>{host.status === "online" ? "No sessions yet" : host.status === "disconnected" ? "Disconnected" : host.status === "error" ? "Connection needs attention" : "Setting up…"}<ArrowUpRight size={12} /></button>}
          </div>}
        </div>)}</nav>
        {!state.loading && entries.length === 0 && <p className="sidebar-empty">{query ? "No matching machines or sessions." : "Your machines will appear here. Connect one to get started."}</p>}
      </div>
      <footer className="sidebar-footer"><button className="button add-machine" onClick={onAdd}><Plus size={17} /> Connect a machine</button><div className="sidebar-meta"><span className={`controller-light ${state.error ? "offline" : ""}`} /><span>{state.error ? "Controller unavailable" : "Personal control plane"}</span><span className="sidebar-version">{state.version || "relay"}</span></div></footer>
    </aside>
  </>;
}
