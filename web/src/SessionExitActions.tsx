import { useRef, useState } from "react";
import { LoaderCircle, Terminal as TerminalIcon, X } from "lucide-react";
import { api, errorMessage } from "./api";
import { InlineError } from "./Forms";
import type { Host, Session } from "./types";

function terminalTitle(title: string) {
  const suffix = " · Terminal";
  const encoder = new TextEncoder();
  let prefix = "";
  let bytes = encoder.encode(suffix).length;
  // Session labels have a 120-byte UTF-8 limit, including the suffix.
  for (const character of title) {
    bytes += encoder.encode(character).length;
    if (bytes > 120) break;
    prefix += character;
  }
  return prefix + suffix;
}

export function SessionExitActions({ host, session, onCreated, onClose }: {
  host: Host; session: Session; onCreated: (session: Session) => void; onClose: () => void;
}) {
  const busy = useRef(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const openTerminal = async () => {
    if (busy.current || host.status !== "online") return;
    busy.current = true; setPending(true); setError(null);
    try {
      onCreated(await api.createSession(host.id, {
        title: terminalTitle(session.title), workspace: session.workspace, cwd: session.cwd, harness: "shell",
      }));
    } catch (error) { setError(errorMessage(error)); }
    finally { busy.current = false; setPending(false); }
  };
  return <section className="session-exit-actions" aria-label="Finished session actions">
    <div className="session-exit-options"><p>Continue in this folder or close this session.</p>
      <button className="button primary compact" disabled={pending || host.status !== "online"} onClick={() => void openTerminal()}>
        {pending ? <LoaderCircle size={15} className="spin" /> : <TerminalIcon size={15} />} {pending ? "Opening terminal…" : "Open terminal here"}
      </button>
      <button className="button secondary compact" disabled={pending || host.status !== "online"} onClick={onClose}><X size={15} /> Close session</button>
    </div>
    <InlineError error={error} />
  </section>;
}
