import { useState } from "react";
import { ArrowRight, ArrowUpRight, Bell, BookOpen, Check, CircleAlert, Folder, LoaderCircle, Plus, Server, Settings2, ShieldCheck, Terminal } from "lucide-react";
import { approvalActionable } from "./Approvals";
import { ObserverSettings, type ObserverActions } from "./Observer";
import { AgentModeBadge } from "./AgentMode";
import { HostStatus } from "./Sidebar";
import type { FleetState, Host, RuntimeState, Session, SessionSummary } from "./types";
import "./Fleet.css";

type Props = {
  state: FleetState;
  observer: ObserverActions;
  onAdd: () => void;
  onHost: (host: Host) => void;
  onActivity: () => void;
  onSession: (host: Host, session: Session) => void;
  onNew: (host: Host) => void;
};
const ordinary = (session: Session) => !session.purpose;
const agentName = (session: Session) => session.harness === "claude" ? "Claude Code" : session.harness === "codex" ? "Codex" : "Shell";
const freshSessions = (host: Host, runtime?: RuntimeState, controllerError = false) => !controllerError && host.status === "online" && Boolean(runtime?.fetchedAt) && !runtime?.sessionsError;
const timestamp = (value?: string) => value && Number.isFinite(Date.parse(value)) ? new Date(value).toLocaleString() : "Not yet sampled";

function summaryStatus(host: Host, runtime?: RuntimeState) {
  if (host.status !== "online") return "Summary state unavailable while offline";
  if (runtime?.observerError) return "Summary state unavailable";
  if (runtime?.observerSupported === false) return "Summaries need a runtime upgrade";
  if (!runtime?.observer) return "Loading summary settings…";
  if (!runtime.observer.config.enabled) return "Summaries off";
  if (runtime.observer.error) return "Summaries need attention";
  if (runtime.observer.running) return "Generating a summary…";
  return `Summaries on · ${runtime.observer.config.model}`;
}

export function FleetOverview({ state, observer, onAdd, onHost, onActivity, onSession, onNew }: Props) {
  const [settings, setSettings] = useState<string | null>(null);
  const [filter, setFilter] = useState<"all" | "running" | "finished">("all");
  const online = state.hosts.filter((host) => host.status === "online");
  const work = state.hosts.flatMap((host) => (state.runtimes[host.id]?.sessions ?? []).filter(ordinary).map((session) => ({ host, session, runtime: state.runtimes[host.id] })));
  const running = work.filter(({ host, runtime, session }) => freshSessions(host, runtime, Boolean(state.error)) && session.status === "running");
  const requests = state.hosts.flatMap((host) => (state.runtimes[host.id]?.approvals ?? []).filter((request) => !state.error && approvalActionable(host, state.runtimes[host.id], request)).map((request) => ({ host, request, session: state.runtimes[host.id].sessions.find((session) => session.id === request.sessionId && session.createdAt === request.sessionCreatedAt)! })));
  const notifications = work.filter(({ session, host }) => session.attention && !requests.some((entry) => entry.host.id === host.id && entry.session.id === session.id && entry.session.createdAt === session.createdAt));
  const unavailable = state.hosts.filter((host) => !freshSessions(host, state.runtimes[host.id], Boolean(state.error)));
  const approvalGaps = state.hosts.filter((host) => state.error || host.status !== "online" || state.runtimes[host.id]?.approvalsError || state.runtimes[host.id]?.approvalsSupported !== true);
  const displayed = work.filter(({ session }) => filter === "all" || (filter === "running" ? session.status === "running" : session.status !== "running"))
    .sort((a, b) => Number(b.session.status === "running") - Number(a.session.status === "running") || Date.parse(b.session.updatedAt) - Date.parse(a.session.updatedAt));
  const settingsHost = state.hosts.find((host) => host.id === settings);
  const settingsRuntime = settingsHost && state.runtimes[settingsHost.id];
  return <div className="page-scroll"><div className="page-content fleet-page work-dashboard">
    <div className="page-eyebrow"><span className="tiny-rule" /> YOUR WORK, ACROSS MACHINES</div>
    <div className="page-heading"><div><h1>Fleet overview</h1><p>Pick up a session. Review what needs you. See the latest context.</p></div><button className="button primary" onClick={onAdd}><Plus size={16} />Connect machine</button></div>
    <div className="metrics" aria-label="Fleet status">
      <Metric icon={<Server size={17} />} label="Machines online" value={state.error ? null : online.length} detail={`of ${state.hosts.length} saved machines`} />
      <Metric icon={<Terminal size={17} />} label="Processes running" value={state.hosts.length > 0 && unavailable.length === state.hosts.length ? null : running.length} detail={unavailable.length ? `${unavailable.length} ${unavailable.length === 1 ? "machine" : "machines"} unverified` : "Confirmed on connected machines"} />
      <Metric icon={<Bell size={17} />} label="Needs review" value={state.error ? null : requests.length + notifications.length} detail="Permission requests & notifications" />
    </div>
    {state.loading ? <div className="empty-card loading-card"><LoaderCircle className="spin" size={24} /><h2>Finding your machines…</h2><p>Connecting to your local Relay controller.</p></div> : !state.hosts.length ? <div className="empty-card fleet-empty"><span className="fleet-empty-icon"><Terminal size={32} /></span><span className="eyebrow">START WITH ONE MACHINE</span><h2>Your next workspace is one connection away.</h2><p>Connect a Linux machine over SSH. Relay keeps your terminal sessions available when you leave and come back.</p><button className="button primary" onClick={onAdd}>Connect your first machine <ArrowRight size={16} /></button><div className="empty-benefits"><span><Check size={14} />SSH keys or password</span><span><Check size={14} />No root required</span></div></div> : <>
      <section className={`fleet-attention ${!requests.length && !notifications.length ? "is-quiet" : ""}`} aria-label="Needs review">
        <div className="section-heading"><div><h2>Needs review <span>{requests.length + notifications.length}</span></h2><p>Review permission requests and agent notifications.</p></div><button className="text-button" onClick={onActivity}>Open Activity <ArrowRight size={14} /></button></div>
        {!!requests.length && <button className="fleet-activity" onClick={onActivity}><ShieldCheck size={18} /><span>{requests.length} pending approval {requests.length === 1 ? "request" : "requests"} · Review in Activity</span><ArrowRight size={16} /></button>}
        {(requests.length > 0 || notifications.length > 0) ? <div className="fleet-attention-list">
          {requests.map(({ host, request, session }) => <div className="fleet-attention-row" key={JSON.stringify([host.id, request.id])}><ShieldCheck size={17} /><div><strong>{request.toolName} needs approval</strong><p>{host.name} · {session.title}</p></div><button className="button secondary compact" onClick={onActivity}>Review request <ArrowRight size={13} /></button></div>)}
          {notifications.map(({ host, session, runtime }) => <div className="fleet-attention-row" key={JSON.stringify([host.id, session.id, session.createdAt])}><Bell size={17} /><div><strong>{session.attention!.kind === "completed" ? "Agent turn finished (reported)" : session.attention!.kind === "permission" ? "Permission notification · inspect terminal" : "Agent notification"}</strong><p>{host.name} · {session.title}{!freshSessions(host, runtime, Boolean(state.error)) && " · Last known notification"}</p></div><button className="button secondary compact" onClick={() => onSession(host, session)}>Open terminal <ArrowUpRight size={13} /></button></div>)}
        </div> : <p className="fleet-quiet"><Check size={16} />No reported requests or notifications.{approvalGaps.length > 0 && <span> Permission requests may still appear in agent terminals.</span>}</p>}
        {approvalGaps.length > 0 && <p className="fleet-footnote">Approval coverage: {approvalGaps.map((host) => `${host.name} (${state.error ? "unavailable" : host.status !== "online" ? "offline" : state.runtimes[host.id]?.approvalsError ? "unavailable" : state.runtimes[host.id]?.approvalsSupported === false ? "terminal only" : "loading"})`).join(" · ")}</p>}
      </section>

      <section className="fleet-work" aria-label="Sessions and progress">
        <div className="section-heading"><div><h2>Sessions & progress <span>{work.length}</span></h2><p>A running process may be working, waiting, or idle. Open its terminal to verify.</p></div>{work.length > 0 && <div className="fleet-filters" role="group" aria-label="Filter sessions">{([['all', 'All'], ['running', 'Running'], ['finished', 'Saved output']] as const).map(([value, label]) => <button key={value} aria-pressed={filter === value} onClick={() => setFilter(value)}>{label}</button>)}</div>}</div>
        {unavailable.length > 0 && <p className="fleet-data-note"><CircleAlert size={14} />Current session state unavailable on {unavailable.map((host) => host.name).join(", ")}. Cached sessions below may be out of date.</p>}
        {!work.length ? <div className="fleet-work-empty"><span className="fleet-empty-icon"><Terminal size={25} /></span><div><h3>{unavailable.length === state.hosts.length ? "Waiting for session state" : unavailable.length ? "No sessions found on connected machines" : "No running sessions"}</h3><p>{unavailable.length === state.hosts.length ? "Open a machine to reconnect or inspect its connection. Existing remote sessions stay on that machine." : "Start an agent or shell on a connected machine to bring its work into view."}</p></div>{online.find((host) => freshSessions(host, state.runtimes[host.id], Boolean(state.error))) && <button className="button primary" onClick={() => onNew(online.find((host) => freshSessions(host, state.runtimes[host.id], Boolean(state.error)))!)}><Plus size={15} />Start a session</button>}</div> : displayed.length ? <div className="fleet-work-grid">{displayed.map(({ host, session, runtime }) => <WorkCard key={JSON.stringify([host.id, session.id, session.createdAt])} host={host} session={session} runtime={runtime} controllerError={Boolean(state.error)} onOpen={() => onSession(host, session)} onActivity={onActivity} onSettings={() => setSettings(host.id)} />)}</div> : <p className="fleet-quiet">{filter === "running" ? "No sessions last reported a running process." : "No saved output yet. Finished sessions will appear here."}</p>}
      </section>

      <section className="machines-section" aria-label="Your machines"><div className="section-heading"><div><h2>Your machines <span>{state.hosts.length}</span></h2><p>Start work, manage coding agents, and choose where summaries run.</p></div></div><div className="machine-grid">{state.hosts.map((host) => {
        const runtime = state.runtimes[host.id];
        const count = runtime?.sessions.filter(ordinary).length ?? 0;
        const agents = runtime?.harnesses.filter((harness) => harness.id !== "shell" && harness.installed && harness.authenticated === true) ?? [];
        return <article className="fleet-machine" key={host.id} aria-label={`${host.name} machine`}><button className="machine-card" onClick={() => onHost(host)}><div className="machine-card-top"><span className="machine-icon"><Server size={21} /></span>{state.error ? <span className="status-pill">Connection unverified</span> : <HostStatus host={host} compact />}</div><h3>{host.name}</h3><p className="mono machine-target">{host.target}</p><div className="machine-card-bottom"><span><Terminal size={13} />{freshSessions(host, runtime, Boolean(state.error)) ? `${count} ${count === 1 ? "session" : "sessions"}` : "Session state unavailable"}</span><ArrowUpRight size={17} /></div>{(host.status === "connecting" || host.status === "installing") && <p className="machine-card-stage"><LoaderCircle className="spin" size={12} />{host.stage || "Reconnecting…"}</p>}{host.runtimeOperation?.status === "running" && <p className="machine-card-stage"><LoaderCircle className="spin" size={12} />{host.runtimeOperation.stage || "Working on the runtime…"}</p>}{host.error && <p className="machine-card-error">{host.error}</p>}</button>
          <div className="fleet-machine-tools"><p className="fleet-agent-readiness">{state.error ? "Agent status unverified" : host.status !== "online" ? "Reconnect to inspect agents" : runtime?.harnessesError ? "Agent status unavailable" : !runtime?.harnesses.length ? "Checking agents…" : agents.length ? `${agents.map((harness) => harness.name).join(" & ")} signed in` : "Install or sign in to a coding agent"}</p><div className="fleet-machine-actions"><button className="button secondary compact" disabled={host.status !== "online" || Boolean(state.error)} onClick={() => onNew(host)}><Plus size={13} />New session</button><button className="text-button" onClick={() => onHost(host)}>{host.status === "online" ? "Manage agents" : "Open machine"}<ArrowRight size={13} /></button></div><div className="fleet-summary-config"><span><BookOpen size={13} />{(state.error ? "Summary state unavailable" : summaryStatus(host, runtime))}</span>{runtime?.observer && <button aria-label={`Summary settings for ${host.name}`} title="Summary settings" className="icon-button" disabled={host.status !== "online" || Boolean(runtime.observerError) || Boolean(state.error)} onClick={() => setSettings(host.id)}><Settings2 size={15} /></button>}</div></div>
        </article>;
      })}</div></section>
      <div className="fleet-summary-note"><BookOpen size={18} /><div><strong>Session summaries</strong><p>Optional snapshots of agent progress, using each machine’s signed-in Claude Code account. Enable them in that machine’s summary settings.</p></div><button className="text-button" onClick={onActivity}>Summary details <ArrowRight size={14} /></button></div>
    </>}
    {settingsHost && settingsRuntime?.observer && <ObserverSettings key={settingsHost.id} host={settingsHost} observer={settingsRuntime.observer} online={settingsHost.status === "online" && !settingsRuntime.observerError && !state.error} actions={observer} onClose={() => setSettings(null)} />}
  </div></div>;
}

function Metric({ icon, label, value, detail }: { icon: React.ReactNode; label: string; value: number | null; detail: string }) {
  return <div className="metric"><div className="metric-label">{icon}<span>{label}</span></div><div className="metric-value">{value == null ? "—" : String(value).padStart(2, "0")}<span>{detail}</span></div></div>;
}

function WorkCard({ host, session, runtime, controllerError, onOpen, onActivity, onSettings }: { host: Host; session: Session; runtime: RuntimeState; controllerError: boolean; onOpen: () => void; onActivity: () => void; onSettings: () => void }) {
  const fresh = freshSessions(host, runtime, controllerError);
  const summary = session.harness !== "shell" ? runtime.observer?.summaries.find((item) => item.sessionId === session.id && item.sessionCreatedAt === session.createdAt) : undefined;
  const stale = summary?.stale || !fresh || Boolean(runtime.observerError) || (summary && summary.sourceStatus !== session.status);
  const waiting = !controllerError && runtime.approvals?.some((request) => request.sessionId === session.id && request.sessionCreatedAt === session.createdAt && approvalActionable(host, runtime, request));
  const summaryCurrent = fresh && !runtime.observerError && !runtime.observer?.error && runtime.observer?.config.enabled;
  const processStatus = !fresh ? "Status unavailable" : session.status === "running" ? "Process running" : session.status === "interrupted" ? "Interrupted" : `Exited${session.exitCode == null ? "" : ` · code ${session.exitCode}`}`;
  return <article className="fleet-work-card" aria-label={`${host.name}: ${session.title}`}>
    <div className="fleet-work-meta"><span className="fleet-work-source">{host.name} · {agentName(session)}<AgentModeBadge session={session} /></span><span className={`fleet-process ${!fresh || session.status === "interrupted" ? "is-stale" : ""}`}><i />{processStatus}</span></div>
    <h3>{session.title}</h3>{waiting && <p className="fleet-waiting"><ShieldCheck size={13} />Waiting for approval</p>}<p className="fleet-work-path"><Folder size={12} /><span>{session.workspace} · <span className="mono">{session.cwd}</span></span></p>
    {session.harness === "shell" ? <p className="fleet-summary-empty">Shell session. Open the terminal to see its output; shells are excluded from summaries.</p> : <>
      <div className="fleet-snapshot-label"><BookOpen size={13} /><span>{summary?.summary ? stale ? "Earlier snapshot · stale" : "Model-generated snapshot" : !fresh || runtime.observerError ? "Summary state unavailable" : summaryCurrent && summary?.status === "running" ? "Summarizing…" : summaryCurrent && summary?.status === "queued" ? "Summary queued" : summary?.status === "error" ? "Summary unavailable" : summaryStatus(host, runtime)}</span></div>
      {summary?.summary ? <><p className="fleet-summary-text">{summary.summary}</p><SnapshotList title="Possible blockers" items={summary.blockers} warning /><SnapshotList title="Suggested next steps" items={summary.nextSteps} /><details className="fleet-reported-steps"><summary>Reported steps{summary.steps.length ? ` · ${summary.steps.length}` : ""}</summary>{summary.steps.length ? <ul>{summary.steps.map((step, index) => <li key={index}>{step}</li>)}</ul> : <p>No steps were included in this snapshot.</p>}</details><p className="fleet-provenance">Claude Code · {summary.model} · Sampled {timestamp(summary.sampledAt)} · Source: {summary.sourceStatus}{stale && <><br />Earlier context; verify current progress in the terminal.</>}</p></> : <p className="fleet-summary-empty">{runtime.observerSupported === false ? "Update this runtime for optional summaries. Its current sessions are preserved." : runtime.observer?.config.enabled ? "No summary is available yet. The process status above does not describe the agent’s progress." : "Enable optional summaries on this machine for a read-only view of its agent sessions."}</p>}
      {summaryCurrent && summary?.summary && (summary.status === "running" || summary.status === "queued") && <p className="fleet-footnote">{summary.status === "running" ? "Generating a newer summary…" : "A newer summary is queued."}</p>}
      {!runtime.observer?.config.enabled && summary?.summary && <p className="fleet-footnote">Summaries are off. This saved snapshot is not being refreshed.</p>}
      {(summary?.error || runtime.observerError || runtime.observer?.error) && <p className="fleet-card-error">{summary?.error || runtime.observerError || runtime.observer?.error}</p>}
    </>}
    {session.harness !== "shell" && runtime.observer?.limits && <p className="fleet-footnote">{runtime.observer.limits.remaining} of {runtime.observer.limits.requestsPerHour} summary requests available this hour.</p>}
    <div className="fleet-work-actions">{waiting && <button className="button secondary compact" onClick={onActivity}><ShieldCheck size={13} />Review request</button>}<button className="button secondary compact" onClick={onOpen}><Terminal size={13} />Open terminal</button>{session.harness !== "shell" && (runtime.observer && !runtime.observer.config.enabled ? <button className="text-button" disabled={!fresh || Boolean(runtime.observerError)} onClick={onSettings}>Set up summaries <Settings2 size={13} /></button> : <button className="text-button" onClick={onActivity}>Summary details <ArrowRight size={13} /></button>)}</div>
  </article>;
}
function SnapshotList({ title, items, warning }: { title: string; items: SessionSummary["steps"]; warning?: boolean }) {
  if (!items.length) return null;
  return <div className={`fleet-snapshot-list ${warning ? "possible-blockers" : ""}`}><h4>{title}</h4><ul>{items.slice(0, 3).map((item, index) => <li key={index}>{item}</li>)}</ul>{items.length > 3 && <details><summary>{items.length - 3} more</summary><ul>{items.slice(3).map((item, index) => <li key={index}>{item}</li>)}</ul></details>}</div>;
}
