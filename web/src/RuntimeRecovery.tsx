import { useEffect, useRef, useState } from "react";
import { AlertTriangle, RefreshCw } from "lucide-react";
import { ApiError, errorMessage, request } from "./api";
import { Modal } from "./Modal";
import type { Host, RuntimeState, Session } from "./types";

type Entry = { source: Session; mode: "reopen" | "resume"; status: string; detail?: string; session?: Session };
type Plan = { runtimeId: string; instanceId: string; fingerprint: string; entries: Entry[]; blockers: string[] };
type Job = { id: string; status: string; stage: string };
type Operation = { id: string; runtimeId: string; status: string; entries: Entry[] };
type Status = { job?: Job | null; operation?: Operation | null; detail?: string };
export function RuntimeRecovery({ host, runtime, onRefresh, onSession }: { host: Host; runtime?: RuntimeState; onRefresh: () => void; onSession: (session: Session) => void }) {
  const [supported, setSupported] = useState<boolean>();
  const [fresh, setFresh] = useState(false);
  const active = host.status === "online" && !runtime?.error;
  const [status, setStatus] = useState<Status>({});
  const [plan, setPlan] = useState<Plan>();
  const [confirmed, setConfirmed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [unknown, setUnknown] = useState(false);
  const pendingIntent = useRef<string | undefined>(undefined);
  const hostIdentity = `${host.id}/${host.createdAt}/${host.target}/${host.port}`;
  const previousHost = useRef(hostIdentity);
  const path = `/api/hosts/${encodeURIComponent(host.id)}/recovery`;
  useEffect(() => {
    if (previousHost.current !== hostIdentity) { previousHost.current = hostIdentity; pendingIntent.current = undefined; setStatus({}); setPlan(undefined); setUnknown(false); setError(undefined); }
    setFresh(false);
    if (!active) return;
    let live = true, timer: ReturnType<typeof setTimeout> | undefined, controller: AbortController | undefined;
    const refresh = async () => {
      if (document.hidden || controller) return;
      controller = new AbortController();
      try {
        const signal = AbortSignal.any([controller.signal, AbortSignal.timeout(8000)]);
        const health = await request<{ capabilities?: Record<string, boolean> }>(`/api/hosts/${encodeURIComponent(host.id)}/runtime/health`, { signal });
        if (!live) return;
        const capability = health.capabilities?.runtimeRecoveryV1 === true;
        setSupported(capability); setFresh(true);
        if (!capability) return;
        const value = await request<Status>(path, { signal });
        if (live) { setStatus(value); if (pendingIntent.current && (value.job?.id === pendingIntent.current || value.operation?.id === pendingIntent.current)) { pendingIntent.current = undefined; setUnknown(false); setPlan(undefined); } }
      } catch { if (live) setFresh(false); /* Keep the last known operation; mutations are never retried. */ }
      finally { controller = undefined; if (live && !document.hidden) timer = setTimeout(refresh, 3000); }
    };
    const visibility = () => { clearTimeout(timer); if (document.hidden) controller?.abort(); else void refresh(); };
    document.addEventListener("visibilitychange", visibility); void refresh();
    return () => { live = false; clearTimeout(timer); controller?.abort(); document.removeEventListener("visibilitychange", visibility); };
  }, [path, active, host.createdAt, host.target, host.port]);
  const review = async () => {
    setBusy(true); setError(undefined);
    try { const value = await request<Plan>(`${path}/preflight`, { signal: AbortSignal.timeout(10000) }); if (!value.runtimeId || !Array.isArray(value.entries) || !Array.isArray(value.blockers)) throw Error("Invalid restart plan"); setPlan(value); setConfirmed(false); }
    catch (cause) { setError(errorMessage(cause)); }
    finally { setBusy(false); }
  };
  const start = async () => {
    if (!plan || !confirmed || busy || unknown || plan.blockers.length) return;
    setBusy(true); setError(undefined); setUnknown(true);
    const id = crypto.randomUUID().replaceAll("-", "");
    pendingIntent.current = id;
    try { const job = await request<Job>(`${path}/start`, { method: "POST", body: JSON.stringify({ id, fingerprint: plan.fingerprint }) }); if (job.id !== id) throw Error("Restart acceptance could not be verified"); setStatus({ job }); setPlan(undefined); pendingIntent.current = undefined; setUnknown(false); onRefresh(); }
    catch (cause) { if (cause instanceof ApiError && cause.status >= 400 && cause.status < 500) { pendingIntent.current = undefined; setUnknown(false); } setError(errorMessage(cause)); }
    finally { setBusy(false); }
  };
  const acknowledge = async () => {
    const id = status.job?.id; if (!id || busy) return;
    setBusy(true); setError(undefined);
    try { await request(`${path}/review`, { method: "POST", body: JSON.stringify({ id }) }); setStatus((current) => ({ ...current, job: current.job ? { ...current.job, status: "reviewed", stage: "Recovery reviewed" } : current.job })); onRefresh(); }
    catch (cause) { setError(errorMessage(cause)); }
    finally { setBusy(false); }
  };
  const unresolved = status.job && status.job.status !== "failed" && status.job.status !== "reviewed";
  const done = status.operation && ["completed", "attention", "reviewed"].includes(status.operation.status);
  return <section className="runtime-maintenance" aria-label="Runtime restart and restore">
    <div className="runtime-maintenance-heading"><span className="quiet-icon"><RefreshCw size={20} /></span><div><h2>Restart and restore</h2><p>Use the installed runtime version. Stop existing processes, then open new terminals for verified saved conversations and shell folders.</p></div><button className="button secondary" disabled={!active || !fresh || !supported || busy || Boolean(unresolved) || unknown} onClick={() => void review()}>Review restart plan</button></div>
    {supported === false && <p className="runtime-offline-note">This runtime cannot safely coordinate a restart yet. Your current sessions remain running. For its first upgrade, finish and close active sessions, stop the Relay daemon on that machine, then reconnect Relay. Later updates can use this workflow.</p>}
    {active && !fresh && <p role="status">Checking the current runtime; restart remains unavailable until this read succeeds.</p>}
    {status.job && <div role="status"><strong>{status.job.stage}</strong>{status.detail && <p>{status.detail}</p>}</div>}
    {status.operation && <div><p>{status.operation.status === "completed" ? "Restoration finished. Verify the new terminals below; no in-flight command or input was replayed." : status.operation.status === "attention" ? "Some sessions need review. Unknown launches were not repeated." : "The runtime is processing the accepted recovery intent."}</p><ul>{status.operation.entries.map((entry) => <li key={`${entry.source.id}/${entry.source.createdAt}`}><strong>{entry.source.title}</strong> — {entry.detail || entry.status}{entry.session && <button className="text-button" onClick={() => onSession(entry.session!)}>Open restored terminal</button>}</li>)}</ul></div>}
    {status.job && (done || status.job.status === "uncertain" || status.job.status === "failed") && status.job.status !== "reviewed" && <button className="button secondary compact" disabled={busy} onClick={() => void acknowledge()}>Mark recovery reviewed</button>}
    {error && <p role="alert" className="inline-error">{error}</p>}
    {unknown && <p role="status">The request outcome is unknown. It will not be sent again. Reconnect this machine and review its recovery status before another restart.</p>}
    {plan && <Modal title="Review runtime restart" subtitle="This replaces processes, not live terminal state." onClose={() => { if (!busy) setPlan(undefined); }}>
      <div className="modal-body"><p><AlertTriangle size={16} /> All listed processes stop. Agent conversations resume only from proven provider IDs; shells reopen their folder. Old terminal records and logs stay available.</p><ul>{plan.entries.map((entry) => <li key={entry.source.id}><strong>{entry.source.title}</strong> — {entry.mode === "reopen" ? "New shell" : "Saved conversation"}{entry.detail && <p>{entry.detail}</p>}</li>)}</ul>{plan.blockers.map((blocker) => <p className="inline-error" role="alert" key={blocker}>{blocker}</p>)}
      <label><input type="checkbox" checked={confirmed} disabled={Boolean(plan.blockers.length) || busy} onChange={(event) => setConfirmed(event.target.checked)} /> I understand that running commands and unsaved terminal state are not restored.</label>{error && <p role="alert">{error}</p>}{unknown && <p role="status">The outcome is unknown. Do not repeat this request.</p>}</div>
      <div className="modal-footer"><button className="button secondary" disabled={busy} onClick={() => setPlan(undefined)}>Cancel</button><button className="button primary" disabled={!confirmed || Boolean(plan.blockers.length) || busy || unknown} onClick={() => void start()}>Restart and restore</button></div>
    </Modal>}
  </section>;
}
