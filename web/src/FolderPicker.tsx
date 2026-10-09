import { useEffect, useId, useRef, useState, type KeyboardEvent } from "react";
import { ArrowUp, Check, ChevronRight, CircleAlert, Folder, FolderOpen, Home, LoaderCircle, X } from "lucide-react";
import { api, errorMessage } from "./api";
import type { DirectoryListing, Host } from "./types";

// Paths are Linux paths on the selected host. Never trim, shell-expand, or
// normalize their names in the browser: spaces and punctuation are literal.
function completion(value: string) {
  if (!value || value === "~" || value.endsWith("/")) return { path: value, prefix: "", delayed: true };
  const slash = value.lastIndexOf("/");
  return { path: slash < 0 ? "" : value.slice(0, slash) || "/", prefix: value.slice(slash + 1), delayed: true };
}

export function FolderPicker({ host, value, onChange, disabled }: { host: Host; value: string; onChange: (path: string) => void; disabled: boolean }) {
  const id = useId();
  const input = useRef<HTMLInputElement>(null);
  const rows = useRef<HTMLDivElement>(null);
  const useFolder = useRef<HTMLButtonElement>(null);
  const focusFirst = useRef(false);
  const [open, setOpen] = useState(false);
  const [location, setLocation] = useState({ path: value, prefix: "", delayed: false });
  const [hidden, setHidden] = useState(false);
  const [listing, setListing] = useState<DirectoryListing | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [home, setHome] = useState("");

  useEffect(() => {
    if (!open) return;
    let active = true;
    const abort = new AbortController();
    setListing(null); setError(null);
    if (host.status !== "online") {
      setLoading(false); setError(`Reconnect ${host.name} to browse its folders.`);
      return () => { active = false; abort.abort(); };
    }
    setLoading(true);
    const timer = window.setTimeout(() => {
      void api.directories(host.id, location.path, location.prefix, hidden, abort.signal)
        .then((result) => { if (active) { setListing(result); setHome(result.home); } })
        .catch((cause) => { if (active) setError(errorMessage(cause)); })
        .finally(() => { if (active) setLoading(false); });
    }, location.delayed ? 220 : 0);
    return () => { active = false; clearTimeout(timer); abort.abort(); };
  }, [host.id, host.name, host.status, open, location, hidden]);

  useEffect(() => {
    if (listing && focusFirst.current) {
      (rows.current?.querySelector<HTMLButtonElement>("button") ?? useFolder.current)?.focus();
      focusFirst.current = false;
    }
  }, [listing]);

  const close = () => { setOpen(false); focusFirst.current = false; input.current?.focus(); };
  const navigate = (path: string) => { setLocation({ path, prefix: "", delayed: false }); setOpen(true); };
  const breadcrumbs = listing?.path.split("/").filter(Boolean).map((name, index, parts) => ({ name, path: "/" + parts.slice(0, index + 1).join("/") })) ?? [];
  const listKeys = (event: KeyboardEvent<HTMLDivElement>) => {
    const buttons = [...(rows.current?.querySelectorAll<HTMLButtonElement>("button") ?? [])];
    const current = buttons.indexOf(document.activeElement as HTMLButtonElement);
    if (current < 0 || !["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    const next = event.key === "Home" ? 0 : event.key === "End" ? buttons.length - 1 : Math.max(0, Math.min(buttons.length - 1, current + (event.key === "ArrowDown" ? 1 : -1)));
    buttons[next]?.focus();
  };

  return <div className="field folder-field" onKeyDown={(event) => {
    if (open && event.key === "Escape") { event.preventDefault(); event.stopPropagation(); close(); }
  }}>
    <label htmlFor={id}>Project folder <span className="required">*</span></label>
    <div className="folder-input-row">
      <input ref={input} id={id} data-autofocus="true" required maxLength={4096} value={value} disabled={disabled} placeholder="/home/you/projects/app" spellCheck={false} autoCapitalize="none" autoCorrect="off" autoComplete="off" aria-describedby={`${id}-hint`} aria-controls={open ? `${id}-browser` : undefined} onChange={(event) => {
        onChange(event.target.value); setLocation(completion(event.target.value)); setOpen(true);
      }} onKeyDown={(event) => {
        if (event.key === "ArrowDown") {
          event.preventDefault();
          if (open && listing) rows.current?.querySelector<HTMLButtonElement>("button")?.focus();
          else { focusFirst.current = true; setLocation(completion(value)); setOpen(true); }
        }
        if (open && event.key === "Enter") { event.preventDefault(); navigate(value); }
      }} />
      <button type="button" className="button secondary folder-browse" aria-label="Browse folders" aria-expanded={open} aria-controls={`${id}-browser`} disabled={disabled || host.status !== "online"} onClick={() => open ? close() : navigate(value)}><FolderOpen size={15} /><span>Browse</span></button>
    </div>
    <small id={`${id}-hint`}>An existing folder on {host.name}. Type a path or browse.</small>
    {open && <section id={`${id}-browser`} className="folder-browser" aria-label={`Folder browser for ${host.name}`} aria-busy={loading}>
      <div className="folder-toolbar">
        <div className="folder-navigation"><button type="button" className="icon-button" aria-label="Home folder" title="Home folder" disabled={disabled || host.status !== "online"} onClick={() => navigate(home)}><Home size={15} /></button><button type="button" className="icon-button" aria-label="Parent folder" title="Parent folder" disabled={disabled || !listing?.parent || loading} onClick={() => listing?.parent && navigate(listing.parent)}><ArrowUp size={15} /></button><span>Folders on {host.name}</span></div>
        <button type="button" className="icon-button" aria-label="Close folder browser" onClick={close}><X size={15} /></button>
      </div>
      {listing && <nav className="folder-breadcrumbs" aria-label="Folder location"><button type="button" onClick={() => navigate("/")} aria-label="Root folder">/</button>{breadcrumbs.map((crumb, index) => <span key={crumb.path}><ChevronRight size={11} /><button type="button" aria-current={index === breadcrumbs.length - 1 ? "location" : undefined} title={crumb.path} onClick={() => navigate(crumb.path)}>{crumb.name}</button></span>)}</nav>}
      {loading ? <div className="folder-message" role="status"><LoaderCircle className="spin" size={17} /><span>Loading folders…</span></div> : error ? <div className="folder-error" role="alert"><CircleAlert size={17} /><div><strong>Couldn’t open this folder</strong><p>{error}</p><button type="button" className="text-button" onClick={() => setLocation({ ...location, delayed: false })}>Try again</button></div></div> : listing && <>
        {location.prefix && <div className="folder-filter">Matching <strong>{location.prefix}</strong></div>}
        <div ref={rows} className="folder-list" onKeyDown={listKeys} role="group" aria-label="Subfolders">
          {listing.directories.length ? listing.directories.map((directory) => <button type="button" key={directory.path} className="folder-entry" aria-label={`Open folder ${directory.name}`} title={directory.path} onClick={() => { focusFirst.current = true; navigate(directory.path); }}><Folder size={16} /><span>{directory.name}</span><ChevronRight size={14} /></button>) : <div className="folder-message"><FolderOpen size={19} /><span>{location.prefix ? "No matching folders." : "This folder has no subfolders."}</span></div>}
        </div>
        {listing.truncated && <p className="folder-truncated">More folders are available. Type more of the path to narrow the list.</p>}
        <div className="folder-selection"><span title={listing.path}>{listing.path}</span><button ref={useFolder} type="button" className="button primary" disabled={disabled} onClick={() => { onChange(listing.path); close(); }}><Check size={14} />Use this folder</button></div>
      </>}
      <label className="folder-hidden"><input type="checkbox" checked={hidden} onChange={(event) => setHidden(event.target.checked)} />Show hidden folders</label>
    </section>}
  </div>;
}
