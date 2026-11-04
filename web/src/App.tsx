import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ArrowLeft, Check, CircleAlert, Menu, Plus, RefreshCw, ShieldCheck, Terminal as TerminalIcon, X } from "lucide-react";
import { api, errorMessage } from "./api";
import { AddHost, Confirm, InlineError, NewSession, SetupHost } from "./Forms";
import { Sidebar } from "./Sidebar";
import { FleetOverview, HostOverview } from "./Views";
import { AgentWindowRequests } from "./AgentWindowRequests";
import { usePeerGroupPublication } from "./PeerSync";
import { SkillsManager } from "./SkillsManager";
import { Activity } from "./Activity";
import { AppearanceControl } from "./AppearanceControl";
import { useApprovalActions } from "./Approvals";
import { useObserverActions } from "./Observer";
import { useFleet } from "./useFleet";
import { useMastermind } from "./useMastermind";
import { canCreateSessions, localCoordinatorOnly } from "./runtimeCapabilities";
import { MastermindWorkspace } from "./MastermindWorkspace";
import { fleetWithoutMaintenance } from "./MaintenanceJobs";
import { TerminalWorkspace } from "./TerminalWorkspace";
import { useTerminalGroups, type TerminalGroup } from "./terminalGroups";
import { MAX_TERMINAL_PANES, paneSelection, readTerminalLayout, terminalLayoutHash, terminalPanes } from "./terminalLayout";
import type { Host, MaintenanceJob, MastermindSection, Selection, Session, TerminalPaneRef } from "./types";

const mastermindSections: Record<MastermindSection, string> = { overview: "Overview", pages: "Pages", tasks: "Tasks", terminal: "Terminal" };

function selectionHash(next: Selection): string {
  const layout = terminalLayoutHash(next);
  if (layout) return layout;
  if (next?.view === "activity" || next?.view === "skills") return next.view;
  if (next?.view === "mastermind") {
    const section = next.section ?? (next.page ? "pages" : "overview");
    // /mastermind/window is the legacy expanded terminal, never an overview.
    const expanded = Boolean(next.expanded && section !== "overview");
    return `mastermind${section === "overview" ? "" : `/${section}`}${section === "pages" && next.page ? `/${encodeURIComponent(next.page)}` : ""}${expanded ? "/window" : ""}`;
  }
  return next?.host ? `host/${encodeURIComponent(next.host)}${next.session ? `/session/${encodeURIComponent(next.session)}${next.expanded ? "/window" : ""}` : ""}` : "";
}

function readSelection(): Selection {
  try {
    if (location.hash.startsWith("#terminals?")) return readTerminalLayout(location.hash.slice(1));
    const parts = location.hash.slice(1).split("/");
    if (parts[0] === "activity" || parts[0] === "skills") return { view: parts[0] };
    if (parts[0] === "mastermind") {
      const route = parts.slice(1);
      const expanded = route.at(-1) === "window";
      if (expanded) route.pop();
      if ((route[0] === "pages" || route[0] === "page") && route.length <= 2) return { view: "mastermind", section: "pages", ...(route[1] ? { page: decodeURIComponent(route[1]) } : {}), ...(expanded ? { expanded: true } : {}) };
      if ((route[0] === "tasks" || route[0] === "terminal") && route.length === 1) return { view: "mastermind", section: route[0], ...(expanded ? { expanded: true } : {}) };
      return { view: "mastermind", section: !route.length && expanded ? "terminal" : "overview", ...(!route.length && expanded ? { expanded: true } : {}) };
    }
    if (parts[0] === "host" && parts[1]) return { host: decodeURIComponent(parts[1]), ...(parts[2] === "session" && parts[3] ? { session: decodeURIComponent(parts[3]), ...(parts[4] === "window" ? { expanded: true } : {}) } : {}) };
  } catch { /* Invalid deep links fall back to the fleet. */ }
  return null;
}

export function App() {
  const { state, refresh, reconnect: reconnectSaved, upsertRuntime, upsertMaintenance } = useFleet();
  const approvalActions = useApprovalActions(refresh);
  const observerActions = useObserverActions(refresh);
  const mastermindActions = useMastermind(state.mastermind, refresh);
  const visibleState = useMemo(() => fleetWithoutMaintenance(state), [state]);
  const [selection, setSelection] = useState<Selection>(readSelection);
  const [mobileOpen, setMobileOpen] = useState(false);
  const [addHost, setAddHost] = useState(false);
  const [setupHost, setSetupHost] = useState<Host | null>(null);
  const [newSession, setNewSession] = useState<Host | null>(null);
  const [newPaneSource, setNewPaneSource] = useState<string | null>(null);
  const savedGroups = useTerminalGroups();
  const peerPublicationError = usePeerGroupPublication(savedGroups);
  const currentSavedGroups = useRef(savedGroups); currentSavedGroups.current = savedGroups;
  const [confirmation, setConfirmation] = useState<{ title: string; description: string; action: string; confirm: () => Promise<void> } | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [toast, setToast] = useState<string | null>(null);
  const host = state.hosts.find((item) => item.id === selection?.host);
  const runtime = host ? state.runtimes[host.id] : undefined;
  const session = runtime?.sessions.find((item) => item.id === selection?.session);
  const importPeerGroups = useCallback((groups: TerminalGroup[]) => {
    if (!groups.length) return;
    const saved = currentSavedGroups.current;
    const distinct = new Set([...saved.groups, ...groups].map((group) => group.id));
    if (!saved.ready || saved.pending || saved.error || distinct.size > 32) {
      const message = "Shared workspace content was imported, but terminal groups could not be saved. Reload saved groups and ensure there is room within the 32-group limit.";
      setActionError(message); throw Error(message);
    }
    for (const group of groups) saved.upsert(group);
  }, []);


  useEffect(() => { const listener = () => setSelection(readSelection()); window.addEventListener("hashchange", listener); return () => window.removeEventListener("hashchange", listener); }, []);
  useEffect(() => {
    if (selection?.view !== "mastermind") return;
    const canonical = `#${selectionHash(selection)}`;
    if (location.hash !== canonical) history.replaceState(history.state, "", `${location.pathname}${location.search}${canonical}`);
  }, [selection]);
  useEffect(() => { if (!toast) return; const timer = setTimeout(() => setToast(null), 5000); return () => clearTimeout(timer); }, [toast]);
  useEffect(() => {
    const viewport = window.visualViewport;
    const resize = () => document.documentElement.style.setProperty("--app-height", `${viewport?.height ?? window.innerHeight}px`);
    resize(); viewport?.addEventListener("resize", resize); window.addEventListener("resize", resize);
    return () => { viewport?.removeEventListener("resize", resize); window.removeEventListener("resize", resize); };
  }, []);

  const navigate = useCallback((next: Selection, replace = false) => {
    setSelection(next); setActionError(null);
    const hash = selectionHash(next);
    if (replace) history.replaceState(history.state, "", `${location.pathname}${location.search}#${hash}`);
    else if (location.hash !== `#${hash}`) location.hash = hash;
  }, []);
  const setTerminalLayout = (panes: TerminalPaneRef[], active: number, expanded: boolean, replace = false) => {
    const current = readSelection();
    const before = terminalPanes(current);
    const pinned = panes.map((pane) => ({ ...pane, createdAt: pane.createdAt ?? state.runtimes[pane.host]?.sessions.find((s) => s.id === pane.session)?.createdAt }));
    const same = before.length === pinned.length && before.every((p, i) => p.host === pinned[i].host && p.session === pinned[i].session && (!p.createdAt || p.createdAt === pinned[i].createdAt));
    let groupId = current?.groupId;
    let saveError: string | undefined;
    // Focus and zoom are view changes. Only deliberate membership changes save.
    if (!same && (groupId || pinned.length > 1)) {
      const existing = savedGroups.groups.find((g) => g.id === groupId);
      if (pinned.every((p) => p.createdAt)) {
        const anchor = pinned[0];
        const session = state.runtimes[anchor.host]?.sessions.find((s) => s.id === anchor.session && s.createdAt === anchor.createdAt);
        groupId ??= crypto.randomUUID();
        const group: TerminalGroup = { id: groupId, title: session?.title ?? existing?.title ?? "Unavailable terminal", hostName: state.hosts.find((h) => h.id === anchor.host)?.name ?? existing?.hostName ?? "Unavailable machine", workspace: session?.workspace || existing?.workspace || "Workspace", panes: pinned as TerminalGroup["panes"], collapsed: existing?.collapsed ?? false };
        savedGroups.upsert(group);
      } else saveError = "This layout is open but could not be saved until every session identity is available.";
    }
    navigate(paneSelection(pinned, active, expanded, groupId), replace);
    if (saveError) setActionError(saveError);
  };
  useEffect(() => {
    const current = readSelection();
    const currentRuntime = current?.host ? state.runtimes[current.host] : undefined;
    if (!currentRuntime?.fetchedAt || currentRuntime.sessionsError) return;
    const currentSession = currentRuntime.sessions.find((item) => item.id === current?.session);
    const completed = state.maintenanceJobs?.find((job) => job.hostId === current?.host && job.sessionId && job.sessionId === current?.session && job.cleanupStatus === "removed" && (!currentSession || currentSession.createdAt === job.sessionCreatedAt));
    if (completed) {
      navigate({ host: completed.hostId });
      setToast(`${completed.harness === "claude" ? "Claude Code" : "Codex"} ${completed.action === "update" ? "updated" : "repaired"} successfully.`);
    }
  }, [state.maintenanceJobs, state.runtimes, selection, navigate]);
  const maintenanceStarted = (job: MaintenanceJob) => { upsertMaintenance(job); refresh(); };
  const perform = async (fn: () => Promise<void>) => { setActionError(null); try { await fn(); refresh(); } catch (error) { setActionError(errorMessage(error)); } };
  const reconnect = (target: Host) => void perform(async () => { await api.connect(target.id); setSetupHost({ ...target, status: "connecting", stage: "Starting SSH connection", error: undefined }); });
  const openedSession = (target: Host, next: Session, notice?: string) => {
    upsertRuntime(target.id, next); setNewSession(null);
    const current = readSelection();
    const panes = terminalPanes(current);
    if (newPaneSource !== null) {
      if (location.hash === newPaneSource && panes.length > 0 && panes.length < MAX_TERMINAL_PANES) {
        setTerminalLayout([...panes, { host: target.id, session: next.id, createdAt: next.createdAt }], panes.length, false);
      } else setToast("Session created. Open it from the machine list when you are ready.");
      setNewPaneSource(null);
    } else navigate({ host: target.id, session: next.id });
    if (notice) setToast(notice);
    refresh();
  };
  const openedFromSession = (target: Host, source: Session, next: Session) => {
    upsertRuntime(target.id, next);
    const current = readSelection();
    const panes = terminalPanes(current);
    const index = panes.findIndex((pane) => pane.host === target.id && pane.session === source.id && (!pane.createdAt || pane.createdAt === source.createdAt));
    if (index >= 0) {
      const active = Math.max(0, panes.findIndex((pane) => pane.host === current?.host && pane.session === current.session));
      setTerminalLayout(panes.map((pane, i) => i === index ? { host: target.id, session: next.id, createdAt: next.createdAt } : pane), active, Boolean(current?.expanded));
    }
    refresh();
  };
  const startSession = (target: Host) => { setNewPaneSource(null); if (localCoordinatorOnly(target, state.runtimes[target.id])) navigate({ view: "mastermind" }); else if (canCreateSessions(target, state.runtimes[target.id])) setNewSession(target); };
  const forget = (target: Host) => setConfirmation({ title: `Forget ${target.name}?`, description: "This removes the machine from this controller. The remote runtime and its sessions will keep running. You can connect it again later.", action: "Forget machine", confirm: async () => { await api.forget(target.id); navigate(null); refresh(); setToast("Machine removed from your fleet."); } });
  const removeSession = (target: Host, current: Session) => {
    const finished = current.status !== "running";
    setConfirmation({ title: `${finished ? "Close" : "End"} “${current.title}”?`, description: finished ? "This removes the saved session and its terminal output. Your project files stay on the machine." : "This terminates the session’s process and removes its saved session record and terminal output. Any work saved to the project folder stays on the machine.", action: finished ? "Close session" : "End and remove", confirm: async () => {
      await api.deleteSession(target.id, current.id);
      const selected = readSelection();
      const panes = terminalPanes(selected);
      const index = panes.findIndex((pane) => pane.host === target.id && pane.session === current.id && (!pane.createdAt || pane.createdAt === current.createdAt));
      if (index >= 0) {
        const remaining = panes.filter((_, i) => i !== index);
        const active = Math.max(0, remaining.findIndex((pane) => pane.host === selected?.host && pane.session === selected.session));
        if (remaining.length) setTerminalLayout(remaining, active, Boolean(selected?.expanded));
        else { if (selected?.groupId) savedGroups.remove(selected.groupId); navigate({ host: target.id }); }
      }
      refresh(); setToast(finished ? "Session closed." : "Session ended and removed.");
    } });
  };

  if (state.unauthorized) return <main className="auth-page"><div className="auth-card"><span className="auth-icon"><ShieldCheck size={30} /></span><span className="eyebrow">YOUR PRIVATE WORKSPACE</span><h1>Reconnect Relay.</h1><p>Choose <strong>Reconnect</strong> in the Relay menu at the top left to restore this window’s connection. Your remote sessions keep running. No Relay account or sign-in is needed.</p><button className="button primary" onClick={refresh}><RefreshCw size={16} /> Check connection</button><AppearanceControl /></div></main>;

  return <div className={`app-shell ${selection?.view === "mastermind" && selection.section === "pages" ? "has-mastermind-page" : ""} ${selection?.expanded ? "terminal-expanded" : ""} ${selection?.view === "mastermind" && selection.section === "pages" && selection.expanded ? "canvas-expanded" : ""} ${selection?.session || (selection?.view === "mastermind" && selection.section === "terminal") ? "has-terminal-workspace" : ""}`}>
    <AgentWindowRequests fleet={visibleState} />
    <Sidebar state={visibleState} terminalGroups={savedGroups.groups} onGroupChange={savedGroups.upsert} onGroupRemove={(id) => { savedGroups.remove(id); if (selection?.groupId === id) navigate({ ...selection, groupId: undefined }, true); }} selected={selection} mobileOpen={mobileOpen} onClose={() => setMobileOpen(false)} onSelect={navigate} onAdd={() => { setMobileOpen(false); setAddHost(true); }} />
    <main className="main-panel">
      <header className="topbar"><button className="icon-button mobile-menu" aria-label="Open navigation" onClick={() => setMobileOpen(true)}><Menu size={20} /></button><div className="topbar-breadcrumb">{selection?.expanded && <button className="text-button" onClick={() => navigate({ ...selection, expanded: false })}><ArrowLeft size={14} />Restore workspace</button>}<TerminalIcon size={16} /><button onClick={() => navigate(null)}>Workspace</button><span>/</span><span>{selection?.view === "skills" ? "Skills" : selection?.view === "activity" ? "Activity" : selection?.view === "mastermind" ? "Mastermind" : host?.name || "Fleet overview"}</span>{selection?.view === "mastermind" && <><span>/</span><span>{mastermindSections[selection.section ?? "overview"]}</span></>}{session && <><span>/</span><span className="topbar-session">{session.title}</span></>}</div><div className="topbar-right"><AppearanceControl /><span className="private-badge"><ShieldCheck size={13} /> Personal</span>{host?.status === "online" && <button className="button compact secondary" disabled={!canCreateSessions(host, runtime) && !localCoordinatorOnly(host, runtime)} onClick={() => startSession(host)}><Plus size={15} /><span>{localCoordinatorOnly(host, runtime) ? "Open Mastermind" : "New session"}</span></button>}</div></header>
      {state.error && <div className="global-alert" role="alert"><CircleAlert size={16} /><span>{state.error} Showing the last available state.</span><button onClick={refresh}>Retry</button></div>}
      {state.reconnectError && <div className="global-alert" role="alert"><CircleAlert size={16} /><span>Could not reconnect saved machines: {state.reconnectError}</span><button disabled={state.reconnecting} onClick={reconnectSaved}>Retry connections</button></div>}
      {savedGroups.error && <div className="global-alert" role="status"><CircleAlert size={16} /><span>{savedGroups.error}</span><button disabled={savedGroups.pending > 0} onClick={() => void savedGroups.reload()}>Reload saved groups</button></div>}
      {peerPublicationError && <div className="global-alert" role="status"><CircleAlert size={16} /><span>{peerPublicationError}</span></div>}
      {actionError && <div className="action-error"><InlineError error={actionError} /><button className="icon-button" onClick={() => setActionError(null)} aria-label="Dismiss error"><X size={15} /></button></div>}
      {!selection ? <FleetOverview state={visibleState} observer={observerActions} mastermind={mastermindActions} onMastermind={() => navigate({ view: "mastermind" })} onAdd={() => setAddHost(true)} onHost={(target) => navigate({ host: target.id })} onActivity={() => navigate({ view: "activity" })} onSession={(target, current) => navigate({ host: target.id, session: current.id })} onNew={startSession} /> : selection.view === "mastermind" ? <MastermindWorkspace fleet={visibleState} actions={mastermindActions} section={selection.section ?? "overview"} pageId={selection.page} expanded={Boolean(selection.expanded)} onSection={(section) => navigate({ view: "mastermind", section, ...(section === "pages" ? { page: selection.page } : {}), expanded: section !== "overview" && selection.expanded })} onPage={(page) => navigate({ view: "mastermind", section: "pages", page, expanded: selection.expanded })} onExpand={() => navigate({ view: "mastermind", section: selection.section === "pages" ? "pages" : "terminal", page: selection.section === "pages" ? selection.page : undefined, expanded: !selection.expanded })} onAdd={() => setAddHost(true)} onHost={(target) => navigate({ host: target.id })} onActivity={() => navigate({ view: "activity" })} onSession={(target, current) => navigate({ host: target.id, session: current.id })} /> : selection.view === "skills" ? <SkillsManager fleet={visibleState} /> : selection.view === "activity" ? <Activity state={visibleState} approvals={approvalActions} observer={observerActions} onSession={(target, current) => navigate({ host: target.id, session: current.id })} /> : selection.session ? <TerminalWorkspace state={state} selection={selection} onLayout={setTerminalLayout} onActivity={() => navigate({ view: "activity" })} onHost={(target) => navigate({ host: target.id })} onEmpty={(hostId) => { if (selection.groupId) savedGroups.remove(selection.groupId); navigate({ host: hostId }); }} onNew={(target) => { setNewPaneSource(location.hash); setNewSession(target); }} onDelete={removeSession} onReviewed={(target, current) => void perform(async () => { await api.acknowledgeAttention(target.id, current.id); })} onCreated={openedFromSession} onRenamed={(target, current) => { upsertRuntime(target.id, current); refresh(); }} /> : !host ? <div className="missing-state"><h1>{state.loading ? "Opening your machine…" : "This machine is unavailable."}</h1><p>{state.loading ? "Fetching your fleet from the controller." : "It may have been removed from this controller."}</p><button className="button secondary" onClick={() => navigate(null)}><ArrowLeft size={16} /> Back to fleet</button></div> : <HostOverview key={host.id} onImportGroups={importPeerGroups} onRefresh={refresh} host={host} runtime={visibleState.runtimes[host.id]} maintenanceJobs={(state.maintenanceJobs ?? []).filter((job) => job.hostId === host.id)} maintenanceSessions={runtime?.sessions ?? []} onMaintenance={maintenanceStarted} onNew={() => startSession(host)} onSession={(current) => navigate({ host: host.id, session: current.id })} onConnect={() => reconnect(host)} onDisconnect={() => void perform(async () => { await api.disconnect(host.id); setToast("Disconnected. Remote sessions keep running."); })} onForget={() => forget(host)} onSetup={() => setSetupHost(host)} onCreated={(current) => openedSession(host, current)} />}
    </main>
    {addHost && <AddHost onClose={() => setAddHost(false)} onCreated={(target) => { setAddHost(false); setSetupHost(target); navigate({ host: target.id }); refresh(); }} />}
    {setupHost && <SetupHost host={state.hosts.find((target) => target.id === setupHost.id) ?? setupHost} onClose={() => setSetupHost(null)} onOpen={() => { navigate({ host: setupHost.id }); setSetupHost(null); refresh(); }} onRetry={() => reconnect(setupHost)} />}
    {newSession && <NewSession host={state.hosts.find((target) => target.id === newSession.id) ?? newSession} harnesses={state.runtimes[newSession.id]?.harnesses ?? []} capabilitiesCurrent={!state.error && !state.runtimes[newSession.id]?.harnessesError && canCreateSessions(newSession, state.runtimes[newSession.id])} existing={visibleState.runtimes[newSession.id]?.sessions ?? []} onClose={() => { setNewSession(null); setNewPaneSource(null); }} onCreated={(current, notice) => openedSession(newSession, current, notice)} />}
    {confirmation && <Confirm title={confirmation.title} description={confirmation.description} action={confirmation.action} onClose={() => setConfirmation(null)} onConfirm={confirmation.confirm} />}
    {toast && <div role="status" className="toast"><Check size={16} /><span>{toast}</span><button aria-label="Dismiss notification" onClick={() => setToast(null)}><X size={14} /></button></div>}
  </div>;
}
