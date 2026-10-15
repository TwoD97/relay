import { Monitor, Moon, Sun, X } from "lucide-react";
import { dismissAppearanceError, useAppearance, type AppearancePreference } from "./appearance";
import "./Appearance.css";

export function AppearanceControl() {
  const { preference, resolved, setPreference, busy, error } = useAppearance();
  const Icon = preference === "system" ? Monitor : resolved === "dark" ? Moon : Sun;
  return <div className="appearance-control">
    <label className="appearance-field" title="Appearance for the app and terminal">
      <Icon size={14} aria-hidden="true" />
      <select aria-label="Appearance" value={preference} disabled={busy} onChange={(event) => setPreference(event.target.value as AppearancePreference)}>
        <option value="system">System</option><option value="light">Light</option><option value="dark">Dark</option>
      </select>
    </label>
    {error && <div className="appearance-notice" role="status"><span>{error}</span><button className="icon-button" aria-label="Dismiss appearance notice" onClick={dismissAppearanceError}><X size={14} /></button></div>}
  </div>;
}
