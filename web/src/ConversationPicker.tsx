import { useEffect, useRef, useState } from "react";
import { Check, LoaderCircle, Pencil, Plus, Search } from "lucide-react";
import { errorMessage, hostPath, request } from "./api";
import "./ConversationPicker.css";

export type Conversation = { id: string; harness: "claude" | "codex"; title: string; titleSource: "provider" | "relay" | "fallback"; cwd: string; updatedAt: string };
type Listing = { cwd: string; harness: string; conversations: Conversation[]; truncated: boolean; resumeSupported: boolean; renameMode: "provider" | "relay"; detail?: string };

export function ConversationPicker({ host, harness, cwd, disabled, resumeSupported, value, onChange }: { host: string; harness: "claude" | "codex"; cwd: string; disabled: boolean; resumeSupported: boolean; value: Conversation | null; onChange: (value: Conversation | null) => void }) {
  const [listing, setListing] = useState<Listing>();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [search, setSearch] = useState("");
  const [editing, setEditing] = useState<Conversation | null>(null);
  const [name, setName] = useState("");
  const [saving, setSaving] = useState(false);
  const generation = useRef(0);
  const [refresh, setRefresh] = useState(0);
  useEffect(() => {
    const current = ++generation.current;
    const abort = new AbortController();
    setListing(undefined); setError(""); setEditing(null); setLoading(Boolean(cwd.trim()));
    if (!cwd.trim()) return;
    const timer = window.setTimeout(() => {
      void request<Listing>(hostPath(host, `conversations?${new URLSearchParams({ harness, cwd })}`), { signal: AbortSignal.any([abort.signal, AbortSignal.timeout(15000)]) })
        .then(data => { if (current === generation.current) setListing(data); })
        .catch(cause => { if (!abort.signal.aborted && current === generation.current) setError(errorMessage(cause)); })
        .finally(() => { if (current === generation.current) setLoading(false); });
    }, 250);
    return () => { ++generation.current; clearTimeout(timer); abort.abort(); };
  }, [host, harness, cwd, refresh]);
  const canResume = resumeSupported && listing?.resumeSupported === true;
  const rename = async () => {
    if (!editing || saving || !name.trim()) return;
    const current = generation.current;
    setSaving(true); setError("");
    try {
      const renamed = await request<Conversation>(hostPath(host, `conversations/${encodeURIComponent(editing.id)}`), { method: "PATCH", body: JSON.stringify({ harness, cwd: listing?.cwd || cwd, title: name.trim() }) });
      if (current !== generation.current) return;
      setListing(previous => previous && ({ ...previous, conversations: previous.conversations.map(item => item.id === renamed.id ? renamed : item) }));
      if (value?.id === renamed.id) onChange(renamed);
      setEditing(null);
    } catch (cause) { if (current === generation.current) setError(errorMessage(cause)); }
    finally { setSaving(false); }
  };
  return <fieldset className="conversation-picker" disabled={disabled}>
    <legend>Conversation</legend>
    <button type="button" className={`conversation-choice ${!value ? "selected" : ""}`} onClick={() => onChange(null)} aria-pressed={!value}><Plus size={16}/><span><strong>New conversation</strong><small>Start with a clean context</small></span>{!value && <Check size={16}/>}</button>
    {!cwd.trim() ? <small>Choose a project folder to find its saved conversations.</small> : <>
      <label className="conversation-search"><Search size={15}/><input aria-label="Search conversations" value={search} onChange={event => setSearch(event.target.value)} placeholder="Find a saved conversation…"/></label>
      {loading && <p role="status"><LoaderCircle size={14} className="spin"/> Loading conversations…</p>}
      {listing && !canResume && <p className="conversation-detail">{listing.detail || "Update and restart this runtime to resume conversations. Fresh sessions are still available."}</p>}
      {error && <div role="alert" className="inline-error">{error}<button type="button" className="button secondary" onClick={() => setRefresh(n => n + 1)}>Refresh list</button></div>}
      <div className="conversation-list">{listing?.conversations.filter(item => `${item.title} ${item.id}`.toLocaleLowerCase().includes(search.toLocaleLowerCase())).map(item => <div key={item.id} className={`conversation-row ${value?.id === item.id ? "selected" : ""}`}>
        <button type="button" className="conversation-select" disabled={!canResume || saving} aria-pressed={value?.id === item.id} onClick={() => onChange(item)}><span><strong>{item.title}</strong><small>{new Date(item.updatedAt).toLocaleString()}{item.titleSource === "relay" ? " · Relay name" : ""}</small></span>{value?.id === item.id && <Check size={16}/>}</button>
        <button type="button" className="icon-button" aria-label={`Rename conversation ${item.title}`} disabled={saving} onClick={() => { setEditing(item); setName(item.title); }}><Pencil size={14}/></button>
      </div>)}</div>
      {!loading && listing?.conversations.length === 0 && <small>No saved conversations in this folder yet.</small>}
      {listing?.truncated && <small>Showing the 100 most recent conversations in this folder.</small>}
      {editing && <div className="conversation-rename"><label className="field"><span>{listing?.renameMode === "relay" ? "Rename in Relay" : "Conversation name"}</span><input aria-label="Conversation name" maxLength={120} value={name} disabled={saving} onChange={event => setName(event.target.value)} onKeyDown={event => { if (event.key === "Enter") { event.preventDefault(); void rename(); } }}/></label><div><button type="button" className="button secondary" disabled={saving} onClick={() => setEditing(null)}>Cancel rename</button><button type="button" className="button primary" disabled={saving || !name.trim()} onClick={() => void rename()}>{saving ? "Saving…" : "Save name"}</button></div>{listing?.renameMode === "relay" && <small>This name is saved on the machine for all Relay clients. Provider history stays unchanged.</small>}</div>}
    </>}
  </fieldset>;
}
