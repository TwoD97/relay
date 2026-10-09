import { useEffect, useRef, useState } from "react";
import { ArrowUpRight, Check, Clock, ShieldCheck, Terminal, X } from "lucide-react";
import { api, ApiError, errorMessage } from "./api";
import type { Approval, FleetState, Host, RuntimeState, Session } from "./types";

type Decision = NonNullable<Approval["decision"]>;
type Attempt = { status: "sending" | "submitted" | "uncertain" | "rejected"; decision: Decision; expiresAt: string; error?: string };
export const approvalKey = (host: string, request: Approval) => JSON.stringify([host, request.sessionId, request.sessionCreatedAt, request.id]);
const storageKey = "relay-approval-attempts";
function readAttempts(): Record<string, Attempt> {
  try {
    const value: unknown = JSON.parse(sessionStorage.getItem(storageKey) ?? "{}");
    if (!value || typeof value !== "object" || Array.isArray(value)) return {};
    return Object.fromEntries(Object.entries(value).filter(([, item]) => item && ["allow", "deny", "terminal"].includes(item.decision) && ["submitted", "uncertain", "rejected"].includes(item.status) && Date.parse(item.expiresAt) > Date.now()).slice(-128));
  } catch { return {}; }
}
export function matchingSession(runtime: RuntimeState | undefined, request: Approval) {
  return runtime?.sessions.find((session) => session.id === request.sessionId && session.createdAt === request.sessionCreatedAt);
}
export function approvalActionable(host: Host, runtime: RuntimeState | undefined, request: Approval, now = Date.now()) {
  return host.status === "online" && !runtime?.sessionsError && !runtime?.approvalsError && runtime?.approvalsSupported === true && matchingSession(runtime, request)?.status === "running" && request.status === "pending" && Date.parse(request.expiresAt) > now;
}
export function pendingApprovals(state: FleetState) {
  return state.hosts.reduce((count, host) => count + (state.runtimes[host.id]?.approvals ?? []).filter((request) => approvalActionable(host, state.runtimes[host.id], request)).length, 0);
}
export function useApprovalActions(refresh: () => void) {
  const [attempts, setAttempts] = useState(readAttempts);
  const ref = useRef(attempts);
  const mounted = useRef(true);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const remember = (key: string, attempt: Attempt) => {
    const entries = Object.entries(ref.current).filter(([id, value]) => id !== key && Date.parse(value.expiresAt) > Date.now()).slice(-127);
    ref.current = { ...Object.fromEntries(entries), [key]: attempt };
    if (mounted.current) setAttempts(ref.current);
    // Persist intent before dispatch. Reloading must not silently offer a second
    // decision for a reply that was lost. Only request identity is stored.
    try { sessionStorage.setItem(storageKey, JSON.stringify(Object.fromEntries(Object.entries(ref.current).map(([id, value]) => [id, value.status === "sending" ? { ...value, status: "uncertain" } : value])))); } catch { /* Memory still prevents duplicate dispatch in this client. */ }
  };
  const decide = async (host: Host, runtime: RuntimeState | undefined, request: Approval, decision: Decision) => {
    const key = approvalKey(host.id, request);
    if (ref.current[key] || !approvalActionable(host, runtime, request)) return false;
    remember(key, { status: "sending", decision, expiresAt: request.expiresAt });
    try {
      const result = await api.decideApproval(host.id, request, decision);
      if (result.id !== request.id || result.sessionId !== request.sessionId || result.sessionCreatedAt !== request.sessionCreatedAt || result.status !== "submitted" || result.decision !== decision) throw new Error("Relay returned an unexpected decision response. Inspect the terminal before doing anything else.");
      remember(key, { status: "submitted", decision, expiresAt: request.expiresAt });
      refresh();
      return true;
    } catch (error) {
      // Even an explicit rejection is not automatically retried. A fresh GET
      // reconciles server facts; inspection remains available throughout.
      remember(key, { status: error instanceof ApiError && error.status >= 400 && error.status < 500 ? "rejected" : "uncertain", decision, expiresAt: request.expiresAt, error: errorMessage(error) });
      refresh();
      return false;
    }
  };
  return { attempts, decide };
}
export type ApprovalActions = ReturnType<typeof useApprovalActions>;
function decisionLabel(decision: Decision | undefined) { return decision === "allow" ? "Approved once" : decision === "deny" ? "Denied" : "Returned to terminal"; }
export function ApprovalCard({ host, runtime, request, actions, onSession }: { host: Host; runtime?: RuntimeState; request: Approval; actions: ApprovalActions; onSession: (host: Host, session: Session) => void }) {
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    setNow(Date.now());
    const delay = Date.parse(request.expiresAt) - Date.now();
    if (request.status !== "pending" || !Number.isFinite(delay) || delay <= 0) return;
    const timer = setTimeout(() => setNow(Date.now()), Math.min(delay + 10, 2147483647));
    return () => clearTimeout(timer);
  }, [request.expiresAt, request.status]);
  const session = matchingSession(runtime, request);
  const attempt = actions.attempts[approvalKey(host.id, request)];
  const expired = request.status === "expired" || (request.status === "pending" && Date.parse(request.expiresAt) <= now);
  const submitted = request.status === "submitted" || attempt?.status === "submitted";
  const uncertain = attempt?.status === "uncertain" && request.status === "pending" && !expired;
  const actionable = !attempt && approvalActionable(host, runtime, request, now);
  const rejected = attempt?.status === "rejected" && request.status === "pending" && !expired;
  const busy = attempt?.status === "sending";
  const label = submitted ? decisionLabel(request.decision ?? attempt?.decision) : busy ? "Sending decision…" : uncertain ? "Decision outcome unknown" : rejected ? "Decision rejected" : expired ? "Request expired" : request.status === "cancelled" ? "Request cancelled" : "Needs approval";
  const openTerminal = async () => {
    if (!session) return;
    const origin = location.hash;
    if (actionable) {
      if (await actions.decide(host, runtime, request, "terminal") && location.hash === origin) onSession(host, session);
    } else if (!busy) onSession(host, session);
  };
  return <article className={`approval-card ${actionable ? "approval-pending" : ""}`} aria-label={`${request.toolName} request for ${session?.title ?? "unavailable session"}`}>
    <div className="approval-heading"><span className="activity-icon"><ShieldCheck size={17} /></span><div><strong>{request.toolName}</strong><p>{request.provider === "claude" ? "Claude Code" : "Codex"} · {session?.title ?? "Session unavailable"}</p></div><span className="activity-state" role="status">{label}</span></div>
    <div className="approval-context"><span className="mono">{request.cwd}</span>{request.permissionMode && <span>Provider mode: {request.permissionMode}</span>}<span><Clock size={12} />{expired ? "Expired" : "Expires"} {new Date(request.expiresAt).toLocaleTimeString()}</span></div>
    <details className="approval-details" open={request.status === "pending"}><summary>Request details</summary><pre>{JSON.stringify(request.input, null, 2)}</pre><p>Request {request.id} · received {new Date(request.createdAt).toLocaleString()}</p></details>
    {request.detail && <p className="activity-note">{request.detail}</p>}
    {submitted && <p className="activity-note">This decision was submitted to the provider hook. Tool execution or completion is not confirmed.</p>}
    {uncertain && <p className="activity-error" role="alert">{attempt?.error || "The previous decision may have been delivered."} Relay will not resend it. Inspect the terminal or wait for updated request state.</p>}
    {rejected && <p className="activity-error" role="alert">{attempt?.error} The decision was rejected and will not be resent. Check the current request or open the terminal.</p>}
    {!submitted && !expired && !busy && !uncertain && !rejected && request.status === "pending" && !actionable && <p className="activity-note">{!session ? "This request no longer matches a current session." : host.status !== "online" ? "Reconnect this machine to check the request." : runtime?.approvalsError || runtime?.sessionsError ? "Request state could not be refreshed. Decisions are disabled until it reconnects." : "This session is no longer running."}</p>}
    <div className="activity-actions">
      {!submitted && !expired && request.status === "pending" && <><button className="button primary compact" disabled={!actionable} onClick={() => void actions.decide(host, runtime, request, "allow")}><Check size={14} />Approve once</button><button className="button secondary compact" disabled={!actionable} onClick={() => void actions.decide(host, runtime, request, "deny")}><X size={14} />Deny</button></>}
      <button className="button secondary compact" disabled={!session || busy} onClick={() => void openTerminal()}><Terminal size={14} />{actionable ? "Continue in terminal" : "Open terminal"}<ArrowUpRight size={12} /></button>
    </div>
    {actionable && <p className="activity-note">Continue in terminal returns this request to the provider’s own prompt without approving or denying it.</p>}
  </article>;
}
export function SessionApprovalNotice({ host, session, runtime, onActivity }: { host: Host; session: Session; runtime?: RuntimeState; onActivity: () => void }) {
  const requests = (runtime?.approvals ?? []).filter((request) => request.sessionId === session.id && request.sessionCreatedAt === session.createdAt && request.status === "pending" && Date.parse(request.expiresAt) > Date.now());
  if (requests.length) return <div className="terminal-banner approval-banner"><span><ShieldCheck size={15} />{requests.length} {requests.length === 1 ? "request needs" : "requests need"} your approval on {host.name}.</span><button onClick={onActivity}>Review requests <ArrowUpRight size={13} /></button></div>;
  if (!session.permissions || session.harness === "shell" || session.purpose) return null;
  return <details className="permission-support"><summary>Permission controls: {session.permissions.support === "active" ? "hook active" : session.permissions.support === "configured" ? "awaiting provider hook" : "terminal only"}</summary><p>{session.permissions.detail}</p>{session.permissions.mode && <p>Last observed provider mode: {session.permissions.mode}{session.permissions.modeObservedAt && ` · ${new Date(session.permissions.modeObservedAt).toLocaleString()}`}</p>}</details>;
}
