import { Check, CircleAlert, LoaderCircle, RefreshCw, Server } from "lucide-react";
import { useState } from "react";
import { api, errorMessage } from "./api";
import type { Host } from "./types";

export function RuntimeMaintenance({ host, onRefresh }: { host: Host; onRefresh: () => void }) {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const operation = host.runtimeOperation;
  const running = pending || operation?.status === "running";
  const repair = async () => {
    if (running) return;
    setPending(true); setError(null);
    try { await api.repairRuntime(host.id); onRefresh(); }
    catch (cause) { setError(errorMessage(cause)); }
    finally { setPending(false); }
  };
  return <section className="runtime-maintenance" aria-label="Relay runtime maintenance">
    <div className="runtime-maintenance-heading"><span className="quiet-icon"><Server size={21} /></span><div><h2>Relay runtime</h2><p>{host.id === "local" ? "Managed by the Relay installation on this computer." : "Update to the bundled version or repair the connection. Your running sessions stay alive."}</p></div>{host.id !== "local" && <button className="button secondary" disabled={host.status !== "online" || running} onClick={() => void repair()}>{running ? <LoaderCircle className="spin" size={14} /> : <RefreshCw size={14} />}{running ? "Working…" : "Update or repair runtime"}</button>}</div>
    {operation && <div className={`runtime-operation operation-${operation.status}`} role={operation.status === "error" ? "alert" : "status"}>
      {operation.status === "running" ? <LoaderCircle className="spin" size={16} /> : operation.status === "error" ? <CircleAlert size={16} /> : <Check size={16} />}
      <div><strong>{operation.stage || (operation.status === "running" ? "Working on the runtime…" : operation.status === "error" ? "Runtime maintenance needs attention" : "Runtime maintenance completed")}</strong>
        {operation.error && <p>{operation.error}</p>}
        {(operation.installedVersion || operation.runningVersion) && <dl className="runtime-versions">{operation.installedVersion && <div><dt>Installed</dt><dd>{operation.installedVersion}</dd></div>}{operation.runningVersion && <div><dt>Running</dt><dd>{operation.runningVersion}</dd></div>}</dl>}
        {operation.restartRequired && <p>The update is installed. Active sessions keep using the running version; a planned runtime restart is needed to use the new version.</p>}
      </div>
    </div>}
    {error && <div className="inline-error" role="alert"><CircleAlert size={16} /><span>{error}</span></div>}
    {host.id !== "local" && host.status !== "online" && <p className="runtime-offline-note">Reconnect this machine before updating or repairing its runtime.</p>}
  </section>;
}
