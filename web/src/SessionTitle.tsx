import { useRef, useState } from "react";
import { Check, LoaderCircle, Pencil, X } from "lucide-react";
import { api, ApiError, errorMessage } from "./api";
import type { Host, Session } from "./types";

export function SessionTitle({ host, session, onSaved }: { host: Host; session: Session; onSaved: (session: Session) => void }) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(session.title);
  const [pending, setPending] = useState(false);
  const [unknown, setUnknown] = useState(false);
  const [error, setError] = useState("");
  const busy = useRef(false);
  const name = draft.trim();
  const invalid = !name || new TextEncoder().encode(name).length > 120 || /\p{Cc}/u.test(name);
  const start = () => { if (host.status !== "online" || busy.current) return; setDraft(session.title); setEditing(true); if (!unknown) setError(""); };
  const save = async () => {
    if (busy.current || invalid || unknown || host.status !== "online") return;
    if (name === session.title) { setEditing(false); return; }
    busy.current = true; setPending(true); setError("");
    try {
      const updated = await api.renameSession(host.id, session.id, name);
      if (updated.id !== session.id || updated.createdAt !== session.createdAt || updated.title !== name) throw new Error("The new terminal name could not be verified.");
      onSaved(updated); setEditing(false);
    } catch (cause) {
      const uncertain = !(cause instanceof ApiError && cause.status >= 400 && cause.status < 500);
      setUnknown(uncertain);
      setError(`${errorMessage(cause)}${uncertain ? " Check the current name before editing again. The rename has not been repeated." : ""}`);
    } finally { busy.current = false; setPending(false); }
  };
  const checkName = async () => {
    if (busy.current) return;
    busy.current = true; setPending(true);
    try {
      const current = (await api.sessions(host.id)).find((item) => item.id === session.id && item.createdAt === session.createdAt);
      if (!current) throw new Error("This terminal is no longer available.");
      onSaved(current); setDraft(current.title); setUnknown(false); setError("");
    } catch (cause) { setError(errorMessage(cause)); }
    finally { busy.current = false; setPending(false); }
  };
  return <div className="session-title-editor">
    {editing ? <form className="session-title-form" onSubmit={(event) => { event.preventDefault(); void save(); }}>
      <input aria-label="Terminal name" value={draft} maxLength={120} autoFocus onFocus={(event) => event.currentTarget.select()} onChange={(event) => setDraft(event.target.value)} disabled={pending || unknown} onKeyDown={(event) => { if (event.key === "Escape" && !pending) { event.preventDefault(); setEditing(false); } }} />
      <button className="icon-button" type="submit" aria-label="Save terminal name" title="Save (Enter)" disabled={pending || invalid || unknown || host.status !== "online"}>{pending ? <LoaderCircle className="spin" size={16} /> : <Check size={16} />}</button>
      <button className="icon-button" type="button" aria-label="Cancel rename" title="Cancel (Escape)" disabled={pending} onClick={() => setEditing(false)}><X size={16} /></button>
    </form> : <div className="session-title-line"><h1><button className="session-title-button" title="Rename terminal" disabled={host.status !== "online" || pending} onClick={start}>{session.title}</button></h1><button className="icon-button" aria-label="Rename terminal" title="Rename terminal" disabled={host.status !== "online" || pending} onClick={start}><Pencil size={14} /></button></div>}
    {editing && !unknown && draft && invalid && <p className="session-rename-error" role="status">{!name ? "Enter a terminal name." : "Use a shorter name without line breaks or control characters."}</p>}
    {error && <p className="session-rename-error" role="alert">{error}</p>}
    {unknown && <button className="text-button" disabled={pending || host.status !== "online"} onClick={() => void checkName()}>Check current name</button>}
  </div>;
}
