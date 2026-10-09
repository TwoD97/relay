import { useRef, useState } from "react";
import { ArrowUpRight, BookOpen, LoaderCircle, RefreshCw, Settings2 } from "lucide-react";
import { api, ApiError, errorMessage } from "./api";
import { Modal } from "./Modal";
import { InlineError } from "./Forms";
import type { FleetState, Host, ObserverConfig, ObserverState, Session, SessionSummary } from "./types";

type Operation = { pending: boolean; error?: string; rejected?: boolean; observedAt?: string };
export function useObserverActions(refresh: () => void) {
  const [operations, setOperations] = useState<Record<string, Operation>>({});
  const inFlight = useRef(new Set<string>());
  const run = async (key: string, operation: () => Promise<ObserverState>, observedAt?: string) => {
    if (inFlight.current.has(key)) return false;
    inFlight.current.add(key);
    setOperations((old) => ({ ...old, [key]: { pending: true, observedAt } }));
    try {
      await operation();
      setOperations((old) => ({ ...old, [key]: { pending: false } }));
      return true;
    } catch (error) {
      // Do not replay an observer mutation after any error. A read refresh is
      // safe; the next explicit action can be chosen after checking state.
      setOperations((old) => ({ ...old, [key]: { pending: false, error: errorMessage(error), rejected: error instanceof ApiError && error.status >= 400 && error.status < 500, observedAt } }));
      return false;
    } finally { inFlight.current.delete(key); refresh(); }
  };
  return { operations, run };
}
export type ObserverActions = ReturnType<typeof useObserverActions>;
const sessionKey = (host: string, session: Session) => JSON.stringify([host, session.id, session.createdAt]);
const isAgentSession = (session: Session) => !session.purpose && (session.harness === "claude" || session.harness === "codex");
function time(value: string | undefined) { return value ? new Date(value).toLocaleString() : "Not yet sampled"; }

export function ObserverSettings({ host, observer, online, actions, onClose }: { host: Host; observer: ObserverState; online: boolean; actions: ObserverActions; onClose: () => void }) {
  const [config, setConfig] = useState<ObserverConfig>({ ...observer.config });
  const [attempted, setAttempted] = useState(false);
  const operation = actions.operations[`${host.id}/config`];
  const pending = operation?.pending;
  const mounted = useRef(true);
  const close = () => { mounted.current = false; onClose(); };
  const save = async (event: React.FormEvent) => {
    event.preventDefault();
    if (pending || attempted || !online) return;
    setAttempted(true);
    if (await actions.run(`${host.id}/config`, () => api.configureObserver(host.id, { ...config, model: config.model.trim() })) && mounted.current) onClose();
  };
  return <Modal title="Summary settings" subtitle={host.name} onClose={close}>
    <form className="observer-settings" onSubmit={(event) => void save(event)}>
      <p>When enabled, Relay periodically sends excerpts from ordinary agent sessions on <strong>{host.name}</strong> to this machine’s authenticated Claude Code CLI. This uses your provider account and may count toward its usage limits. It never approves requests or types into your sessions.</p>
      <label className="observer-enable"><input type="checkbox" checked={config.enabled} disabled={pending || attempted || !online} onChange={(event) => setConfig({ ...config, enabled: event.target.checked })} /><span>Enable session summaries on this machine</span></label>
      <label>Summary provider<select value={config.provider} disabled={pending || attempted || !online} onChange={() => {}}><option value="claude">Claude Code · this machine’s sign-in</option></select></label>
      <p className="activity-note">{observer.providers.find((provider) => provider.id === "codex")?.detail || "Codex is not available as a summary provider because a verified mode with all tools disabled is required. Claude can summarize both Claude Code and Codex sessions."}</p>
      <label>Summary model<input required maxLength={128} value={config.model} disabled={pending || attempted || !online} onChange={(event) => setConfig({ ...config, model: event.target.value })} placeholder="haiku" /></label>
      <label>Check interval (seconds)<input type="number" required min={60} max={3600} step={1} value={config.intervalSeconds} disabled={pending || attempted || !online} onChange={(event) => setConfig({ ...config, intervalSeconds: Number(event.target.value) })} /></label>
      <p className="activity-note">Changed context only, at most one summary at a time and {observer.limits?.requestsPerHour ?? 60} requests per hour on this machine. Login, setup, and maintenance sessions are excluded. Summaries can be incomplete or wrong; use the terminal to verify them.</p>
      <InlineError error={attempted ? operation?.error ?? null : null} />
      {attempted && operation?.error && <p className="activity-note">This request will not be resent. Close settings and check the machine’s reported configuration before making another change.</p>}
      <div className="activity-actions"><button type="button" className="button secondary" onClick={close}>{attempted && operation?.error ? "Close and check state" : "Cancel"}</button><button type="submit" className="button primary" disabled={pending || attempted || !online || !config.model.trim()}>{pending && <LoaderCircle size={14} className="spin" />}{pending ? "Saving…" : "Save settings"}</button></div>
    </form>
  </Modal>;
}
function SummaryCard({ host, session, summary, observer, runtimeError, actions, onSession }: { host: Host; session: Session; summary?: SessionSummary; observer: ObserverState; runtimeError?: string; actions: ObserverActions; onSession: (host: Host, session: Session) => void }) {
  const key = sessionKey(host.id, session);
  const lastOperation = actions.operations[key];
  const operation = lastOperation?.error && summary?.updatedAt && summary.updatedAt !== lastOperation.observedAt ? undefined : lastOperation;
  const busy = operation?.pending || summary?.status === "running" || summary?.status === "queued";
  const stale = summary?.stale || host.status !== "online" || Boolean(runtimeError);
  const status = busy ? summary?.status === "queued" ? "Queued" : "Summarizing…" : summary?.status === "error" ? "Summary unavailable" : summary?.summary ? stale ? "Stale summary" : "Summary available" : "No summary yet";
  return <article className="summary-card" aria-label={`${session.title} summary`}>
    <div className="summary-heading"><div><strong>{session.title}</strong><p>{session.harness === "claude" ? "Claude Code" : "Codex"} · {session.workspace}</p></div><span className={`activity-state ${stale ? "state-stale" : ""}`}>{busy && <LoaderCircle size={12} className="spin" />}{status}</span></div>
    <p className="summary-path mono">{session.cwd}</p>
    {summary?.summary ? <p className="summary-text">{summary.summary}</p> : <p className="activity-note">{observer.config.enabled ? "A summary will appear after this machine processes the session’s context." : "Enable summaries for this machine to get an advisory overview of its agent sessions."}</p>}
    {!!summary?.steps?.length && <SummaryList title="Reported steps" items={summary.steps} />}
    {!!summary?.blockers?.length && <SummaryList title="Possible blockers" items={summary.blockers} />}
    {!!summary?.nextSteps?.length && <SummaryList title="Suggested next steps" items={summary.nextSteps} />}
    {summary && <p className="summary-provenance">Claude Code · {summary.model}<br />Context sampled: {time(summary.sampledAt)}{summary.generatedAt && <><br />Generated: {time(summary.generatedAt)}</>} · Source session: {summary.sourceStatus}</p>}
    {stale && summary?.summary && <p className="activity-note">This describes an earlier snapshot. Check the terminal for current progress.</p>}
    {(summary?.error || operation?.error) && <p className="activity-error" role="alert">{operation?.error || summary?.error}</p>}
    <div className="activity-actions"><button className="button secondary compact" onClick={() => onSession(host, session)}>Open terminal <ArrowUpRight size={13} /></button><button className="button secondary compact" disabled={!observer.config.enabled || host.status !== "online" || Boolean(runtimeError) || busy || observer.running || Boolean(operation?.error && !operation.rejected)} onClick={() => void actions.run(key, () => api.refreshSummary(host.id, session), summary?.updatedAt)}><RefreshCw size={13} />Refresh summary</button></div>
    {operation?.error && <p className="activity-note">{operation.rejected ? "The request was rejected. Resolve the issue, then refresh explicitly when ready." : "Refresh was not repeated. Wait for updated state before requesting another summary."}</p>}
  </article>;
}
function SummaryList({ title, items }: { title: string; items: string[] }) { return <div className="summary-list"><h4>{title}</h4><ul>{items.map((item, index) => <li key={index}>{item}</li>)}</ul></div>; }
export function ObserverOverview({ state, actions, onSession }: { state: FleetState; actions: ObserverActions; onSession: (host: Host, session: Session) => void }) {
  const [settings, setSettings] = useState<string | null>(null);
  const settingsHost = state.hosts.find((host) => host.id === settings);
  const settingsObserver = settingsHost && state.runtimes[settingsHost.id]?.observer;
  return <section className="observer-overview" aria-label="Session summaries"><div className="section-heading"><div><h2><BookOpen size={18} /> Session summaries</h2><p>Optional model-generated snapshots. Verify details in the terminal.</p></div><span className="activity-badge">Read only</span></div>
    {state.hosts.map((host) => {
      const runtime = state.runtimes[host.id];
      const observer = runtime?.observer;
      const sessions = runtime?.sessions.filter(isAgentSession) ?? [];
      return <section className="observer-host" key={host.id} aria-label={`${host.name} summaries`}><div className="activity-host-heading"><div><h3>{host.name}</h3><span>{host.status !== "online" ? "Machine offline" : observer ? observer.config.enabled ? `Enabled · ${observer.config.model} · every ${observer.config.intervalSeconds}s when context changes` : "Summaries off" : runtime?.observerSupported === false ? "Runtime upgrade required" : runtime?.observerError ? "Could not load settings" : "Loading summary settings…"}</span></div>{observer && <button className="button secondary compact" disabled={host.status !== "online" || Boolean(runtime?.observerError)} onClick={() => setSettings(host.id)}><Settings2 size={14} />Settings</button>}</div>
        {runtime?.observerSupported === false && <p className="activity-note">This runtime does not provide summaries. A compatible runtime update is needed; active sessions are preserved.</p>}
        {runtime?.observerError && <p className="activity-error" role="alert">{runtime.observerError} Showing the last available summary state.</p>}
        {observer?.error && <p className="activity-error" role="alert">{observer.error}</p>}
        {observer?.limits && <p className="activity-note">{observer.limits.remaining} of {observer.limits.requestsPerHour} summary requests available this hour.</p>}
        {observer?.running && <p className="activity-note" role="status"><LoaderCircle size={13} className="spin" /> Processing one summary on {host.name}.</p>}
        {observer && (sessions.length ? <div className="summary-grid">{sessions.map((session) => <SummaryCard key={sessionKey(host.id, session)} host={host} session={session} summary={observer.summaries.find((summary) => summary.sessionId === session.id && summary.sessionCreatedAt === session.createdAt)} observer={observer} runtimeError={runtime?.observerError || runtime?.sessionsError} actions={actions} onSession={onSession} />)}</div> : <p className="activity-note">Ordinary Claude Code and Codex sessions appear here. Shells and setup sessions are excluded.</p>)}
      </section>;
    })}
    {settingsHost && settingsObserver && <ObserverSettings key={settingsHost.id} host={settingsHost} observer={settingsObserver} online={settingsHost.status === "online" && !state.runtimes[settingsHost.id]?.observerError} actions={actions} onClose={() => setSettings(null)} />}
  </section>;
}
