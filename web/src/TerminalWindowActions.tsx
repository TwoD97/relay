import { useRef, useState } from "react";
import { Maximize2, Minimize2, AppWindow, LoaderCircle } from "lucide-react";
import { errorMessage } from "./api";
import { openSessionWindow } from "./terminalWindows";

export function TerminalWindowActions({ hostId, sessionId, expanded, onExpand }: { hostId: string; sessionId: string; expanded: boolean; onExpand: () => void }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const pending = useRef(false);
  const open = async () => {
    if (pending.current) return;
    pending.current = true; setBusy(true); setError(undefined);
    try { await openSessionWindow(hostId, sessionId); }
    catch (cause) { setError(`${errorMessage(cause)} The window request has not been repeated.`); }
    finally { pending.current = false; setBusy(false); }
  };
  return <div className="terminal-window-actions">
    <button className="icon-button" aria-label={expanded ? "Restore workspace layout" : "Expand terminal to full window"} title={expanded ? "Restore workspace layout" : "Expand terminal to full window"} onClick={onExpand}>{expanded ? <Minimize2 size={16} /> : <Maximize2 size={16} />}</button>
    <button className="icon-button" aria-label="Open terminal in new window" title="Open terminal in new window" disabled={busy} onClick={() => void open()}>{busy ? <LoaderCircle className="spin" size={16} /> : <AppWindow size={16} />}</button>
    {error && <span className="terminal-window-error" role="alert">{error}</span>}
  </div>;
}
