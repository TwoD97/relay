import { useCallback, useEffect, useMemo, useState } from "react";
import { ArrowLeft, Check, CircleAlert, Menu, Plus, RefreshCw, ShieldCheck, Terminal as TerminalIcon, X } from "lucide-react";
import { api, errorMessage } from "./api";
import { AddHost, Confirm, InlineError, NewSession, SetupHost } from "./Forms";
import { Sidebar } from "./Sidebar";
import { FleetOverview, HostOverview, SessionView } from "./Views";
import { Activity } from "./Activity";
import { useApprovalActions } from "./Approvals";
import { useObserverActions } from "./Observer";
import { useFleet } from "./useFleet";
import { fleetWithoutMaintenance } from "./MaintenanceJobs";
import type { Host, MaintenanceJob, Selection, Session } from "./types";

function readSelection(): Selection {
  try { const parts = location.hash.slice(1).split("/"); return parts[0] === "activity" ? { view: "activity" } : parts[0] === "host" && parts[1] ? { host: decodeURIComponent(parts[1]), ...(parts[2] === "session" && parts[3] ? { session: decodeURIComponent(parts[3]) } : {}) } : null; } catch { return null; }
}

export function App() {
  const { state, refresh, reconnect: reconnectSaved, upsertRuntime, upsertMaintenance } = useFleet();
  const approvalActions = useApprovalActions(refresh);
  const observerActions = useObserverActions(refresh);
  const visibleState = useMemo(() => fleetWithoutMaintenance(state), [state]);
  const [selection, setSelection] = useState<Selection>(readSelection);
  const [mobileOpen, setMobileOpen] = useState(false);
  const [addHost, setAddHost] = useState(false);
  const [setupHost, setSetupHost] = useState<Host | null>(null);
  const [newSession, setNewSession] = useState<Host | null>(null);
  const [confirmation, setConfirmation] = useState<{ title: string; description: string; action: string; confirm: () => Promise<void> } | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [toast, setToast] = useState<string | null>(null);
  const host = state.hosts.find((item) => item.id === selection?.host);
  const runtime = host ? state.runtimes[host.id] : undefined;
  const session = runtime?.sessions.find((item) => item.id === selection?.session);

  useEffect(() => { const listener = () => setSelection(readSelection()); window.addEventListener("hashchange", listener); return () => window.removeEventListener("hashchange", listener); }, []);
  useEffect(() => { if (!toast) return; const timer = setTimeout(() => setToast(null), 5000); return () => clearTimeout(timer); }, [toast]);
  useEffect(() => {
    const viewport = window.visualViewport;
    const resize = () => document.documentElement.style.setProperty("--app-height", `${viewport?.height ?? window.innerHeight}px`);
    resize(); viewport?.addEventListener("resize", resize); window.addEventListener("resize", resize);
    return () => { viewport?.removeEventListener("resize", resize); window.removeEventListener("resize", resize); };
  }, []);

  const navigate = useCallback((next: Selection) => { setSelection(next); setActionError(null); location.hash = next?.view === "activity" ? "activity" : next ? `host/${encodeURIComponent(next.host)}${next.session ? `/session/${encodeURIComponent(next.session)}` : ""}` : ""; }, []);
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
    navigate({ host: target.id, session: next.id });
    if (notice) setToast(notice);
    refresh();
  };
  const openedFromSession = (target: Host, source: Session, next: Session) => {
    upsertRuntime(target.id, next);
    const current = readSelection();
    if (current?.host === target.id && current.session === source.id) navigate({ host: target.id, session: next.id });
    refresh();
  };
  const forget = (target: Host) => setConfirmation({ title: `Forget ${target.name}?`, description: "This removes the machine from this controller. The remote runtime and its sessions will keep running. You can connect it again later.", action: "Forget machine", confirm: async () => { await api.forget(target.id); navigate(null); refresh(); setToast("Machine removed from your fleet."); } });
  const removeSession = (target: Host, current: Session) => {
    const finished = current.status !== "running";
    setConfirmation({ title: `${finished ? "Close" : "End"} “${current.title}”?`, description: finished ? "This removes the saved session and its terminal output. Your project files stay on the machine." : "This terminates the session’s process and removes its saved session record and terminal output. Any work saved to the project folder stays on the machine.", action: finished ? "Close session" : "End and remove", confirm: async () => {
      await api.deleteSession(target.id, current.id);
      const selected = readSelection();
      if (selected?.host === target.id && selected.session === current.id) navigate({ host: target.id });
      refresh(); setToast(finished ? "Session closed." : "Session ended and removed.");
    } });
  };

  if (state.unauthorized) return <main className="auth-page"><div className="auth-card"><span className="auth-icon"><ShieldCheck size={30} /></span><span className="eyebrow">YOUR PRIVATE WORKSPACE</span><h1>Connect this browser to Relay.</h1><p>Open the sign-in link printed by <code>relay ui</code> on your computer. That link connects this browser securely to your controller.</p><button className="button primary" onClick={refresh}><RefreshCw size={16} /> Check connection</button></div></main>;

  return <div className="app-shell">
    <Sidebar state={visibleState} selected={selection} mobileOpen={mobileOpen} onClose={() => setMobileOpen(false)} onSelect={navigate} onAdd={() => { setMobileOpen(false); setAddHost(true); }} />
    <main className="main-panel">
      <header className="topbar"><button className="icon-button mobile-menu" aria-label="Open navigation" onClick={() => setMobileOpen(true)}><Menu size={20} /></button><div className="topbar-breadcrumb"><TerminalIcon size={16} /><button onClick={() => navigate(null)}>Workspace</button><span>/</span><span>{selection?.view === "activity" ? "Activity" : host?.name || "Fleet overview"}</span>{session && <><span>/</span><span className="topbar-session">{session.title}</span></>}</div><div className="topbar-right"><span className="private-badge"><ShieldCheck size={13} /> Personal</span>{host?.status === "online" && <button className="button compact secondary" onClick={() => setNewSession(host)}><Plus size={15} /><span>New session</span></button>}</div></header>
      {state.error && <div className="global-alert" role="alert"><CircleAlert size={16} /><span>{state.error} Showing the last available state.</span><button onClick={refresh}>Retry</button></div>}
      {state.reconnectError && <div className="global-alert" role="alert"><CircleAlert size={16} /><span>Could not reconnect saved machines: {state.reconnectError}</span><button disabled={state.reconnecting} onClick={reconnectSaved}>Retry connections</button></div>}
      {actionError && <div className="action-error"><InlineError error={actionError} /><button className="icon-button" onClick={() => setActionError(null)} aria-label="Dismiss error"><X size={15} /></button></div>}
      {!selection ? <FleetOverview state={visibleState} onAdd={() => setAddHost(true)} onHost={(target) => navigate({ host: target.id })} onActivity={() => navigate({ view: "activity" })} /> : selection.view === "activity" ? <Activity state={visibleState} approvals={approvalActions} observer={observerActions} onSession={(target, current) => navigate({ host: target.id, session: current.id })} /> : !host ? <div className="missing-state"><h1>{state.loading ? "Opening your machine…" : "This machine is unavailable."}</h1><p>{state.loading ? "Fetching your fleet from the controller." : "It may have been removed from this controller."}</p><button className="button secondary" onClick={() => navigate(null)}><ArrowLeft size={16} /> Back to fleet</button></div> : selection.session ? session ? <SessionView key={`${host.id}/${session.id}`} host={host} session={session} runtime={runtime} onActivity={() => navigate({ view: "activity" })} onCreated={(current) => openedFromSession(host, session, current)} onDelete={() => removeSession(host, session)} onHost={() => navigate({ host: host.id })} onReviewed={() => void perform(async () => { await api.acknowledgeAttention(host.id, session.id); })} /> : <div className="missing-state"><h1>{runtime ? "This session is unavailable." : "Loading session…"}</h1><p>{runtime?.error || "Open the machine to see its current sessions."}</p><button className="button secondary" onClick={() => navigate({ host: host.id })}><ArrowLeft size={16} /> Back to machine</button></div> : <HostOverview key={host.id} onRefresh={refresh} host={host} runtime={visibleState.runtimes[host.id]} maintenanceJobs={(state.maintenanceJobs ?? []).filter((job) => job.hostId === host.id)} maintenanceSessions={runtime?.sessions ?? []} onMaintenance={maintenanceStarted} onNew={() => setNewSession(host)} onSession={(current) => navigate({ host: host.id, session: current.id })} onConnect={() => reconnect(host)} onDisconnect={() => void perform(async () => { await api.disconnect(host.id); setToast("Disconnected. Remote sessions keep running."); })} onForget={() => forget(host)} onSetup={() => setSetupHost(host)} onCreated={(current) => openedSession(host, current)} />}
    </main>
    {addHost && <AddHost onClose={() => setAddHost(false)} onCreated={(target) => { setAddHost(false); setSetupHost(target); navigate({ host: target.id }); refresh(); }} />}
    {setupHost && <SetupHost host={state.hosts.find((target) => target.id === setupHost.id) ?? setupHost} onClose={() => setSetupHost(null)} onOpen={() => { navigate({ host: setupHost.id }); setSetupHost(null); refresh(); }} onRetry={() => reconnect(setupHost)} />}
    {newSession && <NewSession host={state.hosts.find((target) => target.id === newSession.id) ?? newSession} harnesses={state.runtimes[newSession.id]?.harnesses ?? []} existing={visibleState.runtimes[newSession.id]?.sessions ?? []} onClose={() => setNewSession(null)} onCreated={(current, notice) => openedSession(newSession, current, notice)} />}
    {confirmation && <Confirm title={confirmation.title} description={confirmation.description} action={confirmation.action} onClose={() => setConfirmation(null)} onConfirm={confirmation.confirm} />}
    {toast && <div role="status" className="toast"><Check size={16} /><span>{toast}</span><button aria-label="Dismiss notification" onClick={() => setToast(null)}><X size={14} /></button></div>}
  </div>;
}
