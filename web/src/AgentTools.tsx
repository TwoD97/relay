import { Check, CircleHelp, Code2, Download, KeyRound, LoaderCircle, RefreshCw, ShieldCheck, Wrench } from "lucide-react";
import type { Harness, HarnessAction } from "./types";

export function AgentTools({ harness, active, pending, fallbackMaintenance, onAction }: { harness: Harness; active: boolean; pending: string | null; fallbackMaintenance: boolean; onAction: (action: HarnessAction) => void }) {
  const actions = harness.management?.supportedActions;
  const canUpdate = actions?.includes("update") || fallbackMaintenance;
  const canRepair = actions?.includes("repair") || fallbackMaintenance;
  const opening = pending === `${harness.id}/install` || pending === `${harness.id}/login`;
  const preparingUpdate = pending === `${harness.id}/update`;
  const preparingRepair = pending === `${harness.id}/repair`;
  const disabled = !active || pending !== null;
  return <article className="harness-card" aria-label={harness.name}>
    <div className="harness-card-top"><span className={`harness-logo ${harness.id}`} aria-hidden="true">{harness.id === "claude" ? "✳" : <Code2 size={24} />}</span><div><h3>{harness.name}</h3><p>{harness.installed ? harness.version || "Installed" : "Not installed"}</p></div>{harness.installed && <Check className="installed-check" size={17} />}</div>
    <div className={`auth-state ${harness.authenticated === true ? "authenticated" : ""}`}>{harness.authenticated === true ? <ShieldCheck size={14} /> : harness.authenticated === false ? <KeyRound size={14} /> : <CircleHelp size={14} />}<span>{!harness.installed ? "Set up this agent to begin" : harness.authenticated === true ? "Authenticated" : harness.authenticated === false ? "Sign-in required" : "Authentication not confirmed"}</span></div>
    {harness.authDetail && <p className="auth-detail">{harness.authDetail}</p>}
    <button className="button secondary harness-action" disabled={disabled} onClick={() => onAction(harness.installed ? "login" : "install")}>{opening ? <LoaderCircle className="spin" size={15} /> : harness.installed ? <KeyRound size={15} /> : <Download size={15} />}{opening ? "Opening terminal…" : harness.installed ? harness.authenticated === true ? "Manage sign-in" : "Sign in" : "Install agent"}</button>
    {(harness.installed || canRepair) && <div className="harness-maintenance-actions">
      {(canUpdate || !actions) && <button className="button secondary" disabled={disabled || !canUpdate} onClick={() => onAction("update")}>{preparingUpdate ? <LoaderCircle className="spin" size={13} /> : <RefreshCw size={13} />}{preparingUpdate ? "Preparing update…" : "Update agent"}</button>}
      {(canRepair || !actions) && <button className="button secondary" disabled={disabled || !canRepair} onClick={() => onAction("repair")}>{preparingRepair ? <LoaderCircle className="spin" size={13} /> : <Wrench size={13} />}{preparingRepair ? "Preparing repair…" : "Repair"}</button>}
    </div>}
    {harness.management?.detail && <p className="harness-management-detail">{harness.management.detail}</p>}
    {fallbackMaintenance && <p className="harness-management-detail">Updates and repairs run in the background and close automatically. Your active sessions and sign-in stay available.</p>}
    {harness.installed && !harness.management && !fallbackMaintenance && <p className="harness-management-detail">Agent update and repair need a newer running Relay runtime. Existing sessions and sign-in are still available.</p>}
  </article>;
}
