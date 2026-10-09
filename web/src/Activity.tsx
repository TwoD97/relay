import { Bell, Check, ShieldCheck, Terminal } from "lucide-react";
import { ApprovalCard, matchingSession, type ApprovalActions } from "./Approvals";
import { ObserverOverview, type ObserverActions } from "./Observer";
import type { FleetState, Host, Session } from "./types";
import "./Activity.css";

export function Activity({ state, approvals, observer, onSession }: { state: FleetState; approvals: ApprovalActions; observer: ObserverActions; onSession: (host: Host, session: Session) => void }) {
  const count = state.hosts.reduce((n, host) => n + (state.runtimes[host.id]?.approvals ?? []).filter((request) => request.status === "pending" && Date.parse(request.expiresAt) > Date.now()).length, 0);
  return <div className="page-scroll"><div className="page-content activity-page">
    <div className="page-eyebrow"><Bell size={13} /> ATTENTION & CONTEXT</div><div className="page-heading"><div><h1>Activity</h1><p>Review exact permission requests. Keep a read-only view of your agents’ work.</p></div></div>
    <section className="approval-inbox" aria-label="Needs approval"><div className="section-heading"><div><h2><ShieldCheck size={18} /> Needs approval <span>{count}</span></h2><p>Decisions apply once to a verified provider hook request.</p></div></div>
      {count === 0 && <div className="activity-empty"><Check size={19} /><div><strong>No pending approval requests.</strong><p>Provider prompts that cannot be handled here remain in their terminals.</p></div></div>}
      {state.hosts.map((host) => {
        const runtime = state.runtimes[host.id];
        const requests = [...(runtime?.approvals ?? [])].sort((a, b) => Number(b.status === "pending") - Number(a.status === "pending") || b.createdAt.localeCompare(a.createdAt));
        const notices = (runtime?.sessions ?? []).filter((session) => !session.purpose && session.harness !== "shell" && (session.attention?.kind === "permission" || session.permissions?.support === "configured" || session.permissions?.support === "terminal-only") && !requests.some((request) => matchingSession(runtime, request) === session && request.status === "pending"));
        return <section key={host.id} className="approval-host" aria-label={`${host.name} approvals`}><div className="activity-host-heading"><div><h3>{host.name}</h3><span>{host.status !== "online" ? "Machine offline · last available state" : runtime?.approvalsSupported === false ? "Terminal permission prompts only" : runtime?.approvalsError ? "Request state unavailable" : runtime?.approvalsSupported ? "Provider requests" : "Checking provider support…"}</span></div></div>
          {runtime?.approvalsSupported === false && <p className="activity-note">This runtime does not expose approval hooks. Review permission prompts in the terminal. A runtime update is required for inbox controls.</p>}
          {runtime?.approvalsError && <p className="activity-error" role="alert">{runtime.approvalsError}</p>}
          {requests.map((request) => <ApprovalCard key={JSON.stringify([request.sessionId, request.sessionCreatedAt, request.id])} host={host} runtime={runtime} request={request} actions={approvals} onSession={onSession} />)}
          {notices.map((session) => <div className="permission-fallback" key={session.id}><div><strong>{session.title}</strong><p>{session.permissions?.detail || "A permission notification was received. Review the provider’s prompt in the terminal."}</p></div><button className="button secondary compact" onClick={() => onSession(host, session)}><Terminal size={14} />Open terminal</button></div>)}
        </section>;
      })}
    </section>
    <ObserverOverview state={state} actions={observer} onSession={onSession} />
  </div></div>;
}
