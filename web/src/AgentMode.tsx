import { useId } from "react";
import { ShieldCheck, Zap } from "lucide-react";
import type { AgentMode, Session } from "./types";
import "./AgentMode.css";

export function AgentModePicker({ value, onChange, autoSupported, current, disabled }: { value: AgentMode; onChange: (mode: AgentMode) => void; autoSupported: boolean; current: boolean; disabled: boolean }) {
  const description = useId();
  return <fieldset className="agent-mode-picker" disabled={disabled} aria-describedby={description}>
    <legend>Agent mode</legend>
    <div className="agent-mode-options">
      <label className={`agent-mode-option ${value === "manual" ? "selected" : ""}`}><input type="radio" name="agent-mode" aria-label="Manual" checked={value === "manual"} onChange={() => onChange("manual")} /><ShieldCheck size={17} /><span><strong>Manual</strong><small>Normal provider permissions</small></span></label>
      <label className={`agent-mode-option mode-auto ${value === "auto" ? "selected" : ""} ${!autoSupported ? "unavailable" : ""}`}><input type="radio" name="agent-mode" aria-label="Auto" checked={value === "auto"} disabled={!autoSupported} onChange={() => onChange("auto")} /><Zap size={17} /><span><strong>Auto</strong><small>Full permissions</small></span></label>
    </div>
    <p id={description} className="agent-mode-description">{value === "auto" ? "Requests full provider permissions, with instructions to work autonomously. Access is limited by this machine’s user account and project or organization restrictions; Auto does not grant sudo." : "Uses the provider’s normal permission settings. Your existing configuration determines when the agent asks for approval."}</p>
    {!autoSupported && <p className="agent-mode-unavailable">{!current ? "Auto is unavailable until Relay can verify this machine’s agent capabilities." : "This runtime does not advertise Auto support. Update the runtime to use Auto for new sessions."}{value === "auto" && " Auto is still selected. Choose Manual or wait for support to be restored before starting."}</p>}
  </fieldset>;
}

export function AgentModeBadge({ session }: { session: Session }) {
  if (session.harness === "shell" || session.purpose) return null;
  const mode = session.agentMode;
  const label = mode === "auto" ? "Auto · full access" : mode === "manual" ? "Manual" : "Provider default";
  const detail = mode === "auto" ? "Started with the provider’s full-permission launch option. Account, project and organization restrictions still apply." : mode === "manual" ? "Started with the provider’s normal permission settings." : "This session did not record a launch mode.";
  return <span className={`agent-mode-badge ${mode === "auto" ? "mode-auto" : ""}`} aria-label={`Launch mode: ${label}`} title={`${detail} This is the launch setting; permissions may change in the agent terminal.`}>{mode === "auto" ? <Zap size={11} /> : <ShieldCheck size={11} />}{label}</span>;
}
