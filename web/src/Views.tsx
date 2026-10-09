import { ArrowRight, ArrowUpRight, Check, CircleAlert, Code2, Folder, KeyRound, Layers3, LoaderCircle, Network, Plus, Power, Radio, RefreshCw, Server, ShieldCheck, Terminal as TerminalIcon, Trash2 } from "lucide-react";
import { useState } from "react";
import { api, errorMessage, runtimePath } from "./api";
import { InlineError } from "./Forms";
import { HostStatus, SessionStatus } from "./Sidebar";
import { Terminal } from "./LazyTerminal";
import { RuntimeMaintenance } from "./RuntimeMaintenance";
import { AgentTools } from "./AgentTools";
import { pendingApprovals, SessionApprovalNotice } from "./Approvals";
import { SessionExitActions } from "./SessionExitActions";
import { MaintenanceJobs, maintenanceActive } from "./MaintenanceJobs";
import type { FleetState, Harness, HarnessAction, Host, MaintenanceJob, RuntimeState, Session } from "./types";

export function FleetOverview({ state, onAdd, onHost, onActivity }: { state: FleetState; onAdd: () => void; onHost: (host: Host) => void; onActivity: () => void }) {
  const online = state.hosts.filter((host) => host.status === "online");
  const running = online.reduce((sum, host) => sum + (state.runtimes[host.id]?.sessions.filter((session) => session.status === "running").length ?? 0), 0);
  const ready = online.reduce((sum, host) => sum + (state.runtimes[host.id]?.harnesses.filter((harness) => harness.id !== "shell" && harness.installed && harness.authenticated === true).length ?? 0), 0);
  return <div className="page-scroll"><div className="page-content fleet-page">
    <div className="page-eyebrow"><span className="tiny-rule" /> YOUR WORK, IN VIEW</div>
    <div className="page-heading"><div><h1>A home for your fleet.</h1><p>Every machine. Every session. Room to focus.</p></div><button className="button primary" onClick={onAdd}><Plus size={16} /> Connect machine</button></div>
    <div className="metrics"><Metric icon={<Server size={18} />} label="Machines online" value={online.length} detail={`of ${state.hosts.length} machines`} /><Metric icon={<TerminalIcon size={18} />} label="Running sessions" value={running} detail="On available machines" /><Metric icon={<Code2 size={18} />} label="Agents authenticated" value={ready} detail="Ready to start work" /></div>
    {pendingApprovals(state) > 0 && <button className="fleet-activity" onClick={onActivity}><ShieldCheck size={18} /><span>{pendingApprovals(state)} pending approval {pendingApprovals(state) === 1 ? "request" : "requests"} · Review in Activity</span><ArrowRight size={16} /></button>}
    {state.loading ? <div className="empty-card loading-card"><LoaderCircle className="spin" size={24} /><h2>Finding your machines…</h2><p>Connecting to your local Relay controller.</p></div> : state.hosts.length === 0 ? <div className="empty-card fleet-empty"><div className="empty-illustration" aria-hidden="true"><span className="orbit orbit-one" /><span className="orbit orbit-two" /><div className="empty-terminal"><div className="mini-window-dots"><i /><i /><i /></div><span className="mini-prompt">~ <b>›</b> <i /></span><span className="mini-line" /><span className="mini-line short" /></div><div className="floating-server"><Server size={21} /><span /></div></div><span className="eyebrow">START WITH ONE MACHINE</span><h2>Your next workspace is one connection away.</h2><p>Connect a Linux machine over SSH. Relay sets up a persistent runtime, so you can leave a terminal and come back to your work.</p><button className="button primary" onClick={onAdd}>Connect your first machine <ArrowRight size={16} /></button><div className="empty-benefits"><span><Check size={14} /> SSH keys or password</span><span><Check size={14} /> No root required</span><span><Check size={14} /> Your infrastructure</span></div></div> : <section className="machines-section"><div className="section-heading"><h2>Your machines <span>{state.hosts.length}</span></h2><span>Live connection status</span></div><div className="machine-grid">{state.hosts.map((host) => {
      const sessions = state.runtimes[host.id]?.sessions ?? [];
      return <button className="machine-card" key={host.id} onClick={() => onHost(host)}><div className="machine-card-top"><span className="machine-icon"><Server size={21} /></span><HostStatus host={host} compact /></div><h3>{host.name}</h3><p className="mono machine-target">{host.target}</p><div className="machine-card-bottom"><span><TerminalIcon size={13} />{sessions.length} {sessions.length === 1 ? "session" : "sessions"}</span><ArrowUpRight size={17} /></div>{(host.status === "connecting" || host.status === "installing") && <p className="machine-card-stage"><LoaderCircle className="spin" size={12} />{host.stage || "Reconnecting…"}</p>}{host.runtimeOperation?.status === "running" && <p className="machine-card-stage"><RefreshCw className="spin" size={12} />{host.runtimeOperation.stage || "Working on the runtime…"}</p>}{host.error && <p className="machine-card-error">{host.error}</p>}</button>;
    })}<button className="machine-card add-card" onClick={onAdd}><span className="add-card-icon"><Plus size={21} /></span><strong>Connect another machine</strong><span>Bring your next workspace into view</span></button></div></section>}
    <div className="principles-strip"><div><Network size={17} /><span>Connected by SSH</span></div><span className="strip-separator" /><div><ShieldCheck size={17} /><span>Runs on your machines</span></div><span className="strip-separator" /><div><Layers3 size={17} /><span>Sessions stay when you leave</span></div></div>
  </div></div>;
}

function Metric({ icon, label, value, detail }: { icon: React.ReactNode; label: string; value: number; detail: string }) {
  return <div className="metric"><div className="metric-label">{icon}<span>{label}</span></div><div className="metric-value">{String(value).padStart(2, "0")}<span>{detail}</span></div></div>;
}

export function HostOverview({ host, runtime, onNew, onSession, onConnect, onDisconnect, onForget, onSetup, onCreated, onRefresh, maintenanceJobs, maintenanceSessions, onMaintenance }: { host: Host; runtime?: RuntimeState; onNew: () => void; onSession: (session: Session) => void; onConnect: () => void; onDisconnect: () => void; onForget: () => void; onSetup: () => void; onCreated: (session: Session) => void; onRefresh: () => void; maintenanceJobs: MaintenanceJob[]; maintenanceSessions: Session[]; onMaintenance: (job: MaintenanceJob) => void }) {
  const active = host.status === "online";
  const sessions = runtime?.sessions ?? [];
  const [pending, setPending] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const harnessAction = async (harness: Harness, action: HarnessAction) => {
    setPending(`${harness.id}/${action}`); setError(null);
    try {
      if (action === "update" || action === "repair") onMaintenance(await api.maintainHarness(host.id, harness.id, action));
      else onCreated(await api.harnessAction(host.id, harness.id, action));
    }
    catch (error) { setError(errorMessage(error)); }
    finally { setPending(null); }
  };
  return <div className="page-scroll"><div className="page-content host-page">
    <div className="page-eyebrow"><Server size={13} /> MACHINE</div><div className="page-heading"><div><div className="host-title-line"><h1>{host.name}</h1><HostStatus host={host} /></div><p className="mono">{host.id === "local" ? "Local runtime" : host.target} {host.id !== "local" && <span className="muted">· {host.port ? `port ${host.port}` : "SSH config defaults"}</span>}</p></div>{active ? <button className="button primary" onClick={onNew}><Plus size={16} /> New session</button> : host.id === "local" ? null : host.status === "connecting" || host.status === "installing" ? <button className="button primary" onClick={onSetup}><TerminalIcon size={16} /> Open setup</button> : <button className="button primary" onClick={onConnect}><RefreshCw size={16} /> Reconnect</button>}</div>
    {!active && <div className={`host-connection-notice ${host.status === "error" ? "has-error" : ""}`}><Radio size={19} /><div><strong>{host.stage || "Machine disconnected"}</strong><p>{host.error || (host.status === "disconnected" ? "Reconnect to see current state and attach to your remote sessions." : host.stage === "Waiting for another host setup" ? "This machine will connect when another setup finishes. You can open its terminal while you wait." : "Relay is reconnecting. Open the setup terminal if SSH needs a password or host verification.")}</p></div>{host.status !== "disconnected" && <button className="text-button" onClick={onSetup}>View terminal <ArrowRight size={14} /></button>}</div>}
    {host.setupWarning && <div className="host-setup-warning" role="status"><CircleAlert size={16} /><div><strong>Agent coordination setup needs attention</strong><p>{host.setupWarning}</p><p>This does not stop your sessions. Runtime repair can retry setup.</p></div></div>}
    <InlineError error={runtime?.error ? `Could not refresh this machine: ${runtime.error}` : null} />
    {(runtime?.error || !active) && runtime?.fetchedAt && <p className="stale-note">Session state last refreshed at {new Date(runtime.fetchedAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })}.</p>}
    <section><div className="section-heading"><h2>Sessions <span>{sessions.length}</span></h2>{active && sessions.length > 0 && <button className="text-button" onClick={onNew}><Plus size={14} /> New session</button>}</div>{sessions.length > 0 ? <div className="sessions-list">{sessions.map((session) => <button className="session-card" onClick={() => onSession(session)} key={session.id}><span className="session-card-icon"><TerminalIcon size={18} /></span><div className="session-card-title"><strong>{session.title}</strong><span><Folder size={12} />{session.workspace}<b>·</b><span className="mono">{session.cwd}</span></span></div><SessionStatus session={session} /><ArrowUpRight className="session-open-icon" size={17} /></button>)}</div> : <div className="sessions-empty"><span className="quiet-icon"><TerminalIcon size={25} /></span><div><h3>{active ? "Make room for your next idea." : "No sessions to show yet."}</h3><p>{active ? "Open a shell or start a coding agent in a project folder." : "Sessions will appear when this machine is connected."}</p></div>{active && <button className="button secondary" onClick={onNew}><Plus size={15} /> Start a session</button>}</div>}</section>
    <section className="harness-section"><div className="section-heading"><div><h2>Coding agents</h2><p>Install once. Sign in on this machine. Start working.</p></div><span className="host-scope"><Server size={13} />{host.name}</span></div><InlineError error={error} />{!runtime?.harnesses.length ? <div className="harness-placeholder">{active ? "Checking installed coding agents…" : "Connect this machine to inspect its coding agents."}</div> : <div className="harness-grid">{runtime.harnesses.filter((harness) => harness.id !== "shell").map((harness) => <AgentTools key={harness.id} harness={harness} fallbackMaintenance={host.id !== "local" && !harness.management} active={active} pending={pending ?? (maintenanceJobs.some((job) => job.harness === harness.id && maintenanceActive(job)) ? `${harness.id}/background` : null)} onAction={(action) => void harnessAction(harness, action)} />)}</div>}<div className="agent-coordination"><Network size={16} /><div><strong>Coordinate sessions from your agent</strong><p>Use <code>$relay</code> in Codex or <code>/relay</code> in Claude Code. Other machines use this host’s existing SSH access.</p></div></div></section>
    <MaintenanceJobs host={host} jobs={maintenanceJobs} sessions={maintenanceSessions} onSession={onSession} />
    <RuntimeMaintenance host={host} onRefresh={onRefresh} />
    {host.id !== "local" && <div className="host-settings-row"><div><h3>Connection</h3><p>Disconnecting keeps remote sessions running.</p></div><div className="host-settings-actions">{active && <button className="button secondary" onClick={onDisconnect}><Power size={14} /> Disconnect</button>}<button className="button ghost-danger" onClick={onForget}><Trash2 size={14} /> Forget machine</button></div></div>}
  </div></div>;
}

export function SessionView({ host, session, runtime, onActivity, onDelete, onHost, onReviewed, onCreated }: { host: Host; session: Session; runtime?: RuntimeState; onActivity: () => void; onDelete: () => void; onHost: () => void; onReviewed: () => void; onCreated: (session: Session) => void }) {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const installed = session.purpose === "install" && session.status === "exited" && session.exitCode === 0;
  const maintained = (session.purpose === "update" || session.purpose === "repair") && session.status === "exited" && session.exitCode === 0;
  const harnessName = session.harness === "claude" ? "Claude Code" : "Codex";
  const signIn = async () => {
    setPending(true); setError(null);
    try { onCreated(await api.harnessAction(host.id, session.harness, "login")); }
    catch (error) { setError(errorMessage(error)); }
    finally { setPending(false); }
  };
  return <div className="session-view"><div className="session-heading"><div><div className="session-location"><button onClick={onHost}>{host.name}</button><span>/</span><span>{session.workspace}</span></div><h1>{session.title}</h1></div><SessionStatus session={session} /><button className="icon-button delete-session" title={session.status === "running" ? "End and remove session" : "Remove saved session"} aria-label={session.status === "running" ? "End and remove session" : "Remove saved session"} onClick={onDelete}><Trash2 size={17} /></button></div><div className="session-path"><Folder size={13} /><span>{session.cwd}</span><span className="session-harness">{session.harness === "shell" ? "Shell" : session.harness === "claude" ? "Claude Code" : "Codex"}</span></div>
    <SessionApprovalNotice host={host} session={session} runtime={runtime} onActivity={onActivity} />
    {session.attention && <div className="terminal-banner attention-banner"><span>{session.attention.kind === "completed" ? "The agent reported completion." : session.attention.kind === "permission" ? "The agent reported a permission request. Review it in the terminal." : "The agent sent a notification. Review its terminal output."} <small>{session.attention.source === "claude-hook" ? "Claude hook" : session.attention.source === "codex-hook" ? "Codex hook" : "Codex notification"}</small></span><button onClick={onReviewed} disabled={host.status !== "online"}>Mark reviewed <Check size={13} /></button></div>}
    {host.status !== "online" && <div className="terminal-banner">This machine is {host.status}. Reconnect from the machine page to resume this terminal.<button onClick={onHost}>Open machine <ArrowRight size={13} /></button></div>}
    {installed && <div className="terminal-banner success-banner" role="status"><span><Check size={15} /> {harnessName} installed successfully. Sign in to use it on this machine.</span><button disabled={pending || host.status !== "online"} onClick={() => void signIn()}>{pending ? <LoaderCircle className="spin" size={13} /> : <KeyRound size={13} />}{pending ? "Opening sign-in…" : "Sign in"}</button><button onClick={onHost}>Open machine <ArrowRight size={13} /></button></div>}
    {maintained && <div className="terminal-banner success-banner" role="status"><span><Check size={15} />{harnessName} {session.purpose === "update" ? "updated" : "repaired"} successfully. New sessions use this installation; active sessions and sign-in are preserved.</span><button onClick={onHost}>Open machine <ArrowRight size={13} /></button></div>}
    <InlineError error={error} />
    {!installed && !maintained && session.status !== "running" && <div className="terminal-banner">{session.status === "interrupted" ? "This process was interrupted by a runtime restart. Its saved output is available below." : `This process has exited${session.exitCode == null ? "." : ` with code ${session.exitCode}.`} Its saved output is available below.`}</div>}
    {session.status !== "running" && !session.purpose && <SessionExitActions host={host} session={session} onCreated={onCreated} onClose={onDelete} />}
    <Terminal key={`${host.id}/${session.id}`} path={runtimePath(host.id, `sessions/${encodeURIComponent(session.id)}/terminal`)} enabled={host.status === "online"} finished={session.status !== "running"} label={`${session.title} terminal`} />
  </div>;
}
