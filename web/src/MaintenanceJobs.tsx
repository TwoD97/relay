import { Check, CircleAlert, LoaderCircle, Terminal as TerminalIcon } from "lucide-react";
import type { FleetState, Host, MaintenanceJob, Session } from "./types";

export function maintenanceActive(job: MaintenanceJob) {
  return job.status === "preparing" || job.status === "starting" || job.status === "running";
}

export function maintenanceNeedsAttention(job: MaintenanceJob) {
  return job.status === "failed" || job.status === "uncertain" || job.cleanupStatus === "uncertain";
}

// Worker sessions remain available to the output viewer, but are not ordinary
// workspaces. Ownership comes from the controller's durable record, never titles.
export function fleetWithoutMaintenance(state: FleetState): FleetState {
  const owned = new Set((state.maintenanceJobs ?? []).filter((job) => job.sessionId && job.sessionCreatedAt)
    .map((job) => JSON.stringify([job.hostId, job.sessionId, job.sessionCreatedAt])));
  return { ...state, runtimes: Object.fromEntries(Object.entries(state.runtimes).map(([host, runtime]) => [host, {
    ...runtime, sessions: runtime.sessions.filter((session) => !owned.has(JSON.stringify([host, session.id, session.createdAt]))),
  }])) };
}

export function MaintenanceJobs({ host, jobs, sessions, onSession }: {
  host: Host; jobs: MaintenanceJob[]; sessions: Session[]; onSession: (session: Session) => void;
}) {
  const latest = new Set<string>();
  const visible = [...jobs].sort((a, b) => b.createdAt.localeCompare(a.createdAt)).filter((job) => {
    const seen = latest.has(job.harness);
    latest.add(job.harness);
    return !seen || maintenanceActive(job) || maintenanceNeedsAttention(job) || job.cleanupStatus === "pending";
  });
  if (!visible.length) return null;
  return <section className="agent-maintenance" aria-label="Agent maintenance">
    <div className="section-heading"><div><h2>Agent maintenance</h2><p>Updates run in the background. Successful jobs close automatically.</p></div></div>
    {visible.map((job) => {
      const name = job.harness === "claude" ? "Claude Code" : "Codex";
      const working = maintenanceActive(job) || job.cleanupStatus === "pending";
      const attention = maintenanceNeedsAttention(job);
      const output = job.cleanupStatus !== "removed" && sessions.find((session) => session.id === job.sessionId && session.createdAt === job.sessionCreatedAt);
      return <article key={job.id} className={`maintenance-job ${attention ? "maintenance-error" : ""}`} aria-label={`${name} ${job.action}`}>
        <div className="maintenance-job-state" role={attention ? "alert" : "status"}>
          {working ? <LoaderCircle size={17} className="spin" /> : attention ? <CircleAlert size={17} /> : <Check size={17} />}
          <div><strong>{name} {job.status === "succeeded" ? job.action === "update" ? "updated successfully" : "repaired successfully" : job.action === "update" ? "update" : "repair"}</strong>
            <p>{job.stage}</p>{job.error && <p>{job.error}</p>}
            {job.cleanupStatus === "uncertain" && <p>Could not confirm cleanup. Saved output is retained until its state can be checked.</p>}
            {host.status !== "online" && working && <p>Reconnect to check progress. Work already started continues on the machine.</p>}
          </div>
        </div>
        {output && <button className="button compact secondary" disabled={host.status !== "online"} onClick={() => onSession(output)}><TerminalIcon size={14} /> View output</button>}
      </article>;
    })}
  </section>;
}
