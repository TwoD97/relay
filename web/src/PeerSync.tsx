import { useCallback, useEffect, useRef, useState } from "react";
import { Copy, RefreshCw, Share2 } from "lucide-react";
import { ApiError, errorMessage, request } from "./api";
import { Confirm, InlineError } from "./Forms";
import { validGroup, type TerminalGroup } from "./terminalGroups";
import type { MastermindWorkspaceState } from "./types";
import "./PeerSync.css";

type Source = { sourceId: string; name: string; revision: number; digest: string; updatedAt: string; groups: number; pages: number; tasks: number; lessons: number; hosts: string[]; conflict?: string };
type Snapshot = { sources: Source[]; warnings: string[]; workspaceRevision: number };
type Imported = { workspace: MastermindWorkspaceState; groups: TerminalGroup[]; warnings: string[] };
type Intent = { id: string; sourceId: string; digest: string; revision: number };
const intentKey = "relay.peer-import.v1";
const hex = (value: unknown, size: number) => typeof value === "string" && new RegExp(`^[a-f0-9]{${size}}$`).test(value);
const count = (value: unknown): value is number => Number.isSafeInteger(value) && Number(value) >= 0;
function snapshot(value: unknown): Snapshot {
  const data = value as Snapshot;
  if (!data || !count(data.workspaceRevision) || !Array.isArray(data.sources) || data.sources.length > 64 || !Array.isArray(data.warnings) || !data.warnings.every((item) => typeof item === "string") || !data.sources.every((item) => item && hex(item.sourceId, 48) && hex(item.digest, 64) && typeof item.name === "string" && count(item.revision) && [item.groups, item.pages, item.tasks, item.lessons].every(count) && Array.isArray(item.hosts) && item.hosts.every((host) => typeof host === "string") && typeof item.updatedAt === "string" && (!item.conflict || typeof item.conflict === "string"))) throw Error("Shared workspace metadata could not be verified.");
  return data;
}
function rememberedIntent(): Intent | undefined {
  try {
    const raw = sessionStorage.getItem(intentKey);
    if (!raw) return;
    const value = JSON.parse(raw) as Intent;
    if (hex(value.id, 32) && hex(value.sourceId, 48) && hex(value.digest, 64) && count(value.revision)) return value;
  } catch { /* Import records are optional until an explicit write is made. */ }
}

// Only confirmed native settings are published. Mark each revision before the
// write: a lost response cannot retry or export an optimistic layout.
export function usePeerGroupPublication(saved: { ready: boolean; pending: number; error: string | null; revision: number; groups: TerminalGroup[] }) {
  const [error, setError] = useState<string>();
  const attempted = useRef<string | undefined>(undefined);
  const queued = useRef<typeof saved | undefined>(undefined);
  const busy = useRef(false);
  const alive = useRef(true);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  useEffect(() => {
    const desktop = window as Window & { __RELAY_PRIMARY_WINDOW__?: boolean; __TAURI_INTERNALS__?: unknown };
    if (!desktop.__TAURI_INTERNALS__ || desktop.__RELAY_PRIMARY_WINDOW__ !== true || !saved.ready || saved.pending || saved.error) return;
    queued.current = saved;
    const publish = async () => {
      if (busy.current || !queued.current || !alive.current) return;
      const value = queued.current; queued.current = undefined;
      const key = JSON.stringify([value.revision, value.groups]);
      if (attempted.current === key) return;
      attempted.current = key; busy.current = true;
      try {
        await request("/api/peer-sync/groups", { method: "POST", body: JSON.stringify({ revision: value.revision, groups: value.groups }) });
        if (alive.current) setError(undefined);
      } catch {
        if (alive.current) setError("Your terminal groups are saved on this computer, but sharing their latest layout was not confirmed. The upload was not repeated.");
      } finally { busy.current = false; if (alive.current && queued.current) void publish(); }
    };
    void publish();
  }, [saved.ready, saved.pending, saved.error, saved.revision, saved.groups]);
  return error;
}

export function PeerSync({ onImportGroups, onRefresh }: { onImportGroups: (groups: TerminalGroup[]) => void; onRefresh: () => void }) {
  const [data, setData] = useState<Snapshot>();
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [reading, setReading] = useState(false);
  const [fresh, setFresh] = useState(false);
  const [selected, setSelected] = useState<Source>();
  const [intent, setIntent] = useState<Intent | undefined>(rememberedIntent);
  const [result, setResult] = useState<string>();
  const [warnings, setWarnings] = useState<string[]>([]);
  const mounted = useRef(true), generation = useRef(0);
  const readController = useRef<AbortController | undefined>(undefined);
  const reload = useCallback(async () => {
    const version = ++generation.current;
    readController.current?.abort();
    const controller = new AbortController(); readController.current = controller;
    setReading(true); setFresh(false);
    try {
      const value = snapshot(await request("/api/peer-sync", { signal: AbortSignal.any([controller.signal, AbortSignal.timeout(10000)]) }));
      if (mounted.current && version === generation.current) { setData(value); setFresh(true); setError(undefined); }
    } catch (cause) { if (mounted.current && version === generation.current) setError(errorMessage(cause)); }
    finally { if (mounted.current && version === generation.current) setReading(false); }
  }, []);
  useEffect(() => { mounted.current = true; void reload(); return () => { mounted.current = false; generation.current++; readController.current?.abort(); }; }, [reload]);
  const importSource = async () => {
    if (!selected || !data || !fresh || busy || intent) return;
    const source = data.sources.find((item) => item.sourceId === selected.sourceId && item.digest === selected.digest);
    if (!source) throw Error("This shared snapshot changed. Refresh and review its current contents.");
    const next = { id: crypto.randomUUID().replaceAll("-", ""), sourceId: source.sourceId, digest: source.digest, revision: data.workspaceRevision };
    sessionStorage.setItem(intentKey, JSON.stringify(next));
    setIntent(next); setBusy(true); setError(undefined); setResult(undefined); setWarnings([]);
    try {
      const value = await request<Imported>("/api/peer-sync/import", { method: "POST", body: JSON.stringify(next) });
      if (!value?.workspace || !count(value.workspace.revision) || value.workspace.revision <= next.revision || !Array.isArray(value.groups) || !value.groups.every(validGroup) || !Array.isArray(value.warnings) || !value.warnings.every((item) => typeof item === "string")) throw Error("The import response could not be verified. Review the current workspace before another copy.");
      sessionStorage.removeItem(intentKey);
      let groupError: string | undefined;
      try { onImportGroups(value.groups); }
      catch (cause) { groupError = `Workspace content was imported, but its terminal groups were not saved: ${errorMessage(cause)}`; }
      onRefresh();
      if (!mounted.current) return;
      setIntent(undefined); setSelected(undefined); setWarnings([...value.warnings, ...(groupError ? [groupError] : [])]);
      setData((current) => current ? { ...current, workspaceRevision: value.workspace.revision } : current);
      setResult("A separate copy was imported. Existing pages, tasks, and running sessions were preserved.");
    } catch (cause) {
      if (cause instanceof ApiError && cause.status >= 400 && cause.status < 500) { sessionStorage.removeItem(intentKey); if (mounted.current) setIntent(undefined); }
      if (mounted.current) { setError(errorMessage(cause)); setFresh(false); setSelected(undefined); }
    } finally { if (mounted.current) setBusy(false); }
  };
  const resume = () => {
    if (!fresh || reading || busy) return;
    sessionStorage.removeItem(intentKey); setIntent(undefined);
    setResult("The current metadata was refreshed. Check Pages, Tasks, and saved groups before making another copy; the earlier request may still finish.");
  };
  return <section className="peer-sync" aria-label="Shared workspaces">
    <div className="peer-sync-heading"><Share2 size={19} /><div><h2>Shared workspaces</h2><p>Other Relay desktops connected to the same machines can share pages, tasks, lessons, and terminal layouts. Import an independent copy when you need it.</p></div><button className="button secondary compact" disabled={reading || busy} onClick={() => void reload()}><RefreshCw size={14} /> Refresh shared workspaces</button></div>
    <p className="peer-sync-boundary">Imports do not transfer sign-in credentials, SSH access, inbox instructions, or terminal ownership. Sessions stay on their original machines.</p>
    <InlineError error={error ?? null} />
    {intent && <div className="peer-sync-unknown" role="status"><strong>The last import has not been confirmed.</strong><p>It will not be repeated automatically. Refresh metadata and inspect your workspace. Request <code>{intent.id}</code>; previous workspace revision {intent.revision}.</p><button className="button secondary compact" disabled={!fresh || reading || busy} onClick={resume}>Review and resume importing</button></div>}
    {result && <p role="status">{result}</p>}
    {[...(data?.warnings ?? []), ...warnings].map((warning, index) => <p className="peer-sync-warning" key={`${index}/${warning}`} role="status">{warning}</p>)}
    {!data && !error && <p role="status">Checking shared workspace snapshots…</p>}
    {data?.sources.length === 0 && <p className="muted">No shared workspace snapshots are available. Connect another Relay desktop to one of these machines to discover its published metadata.</p>}
    <div className="peer-sync-sources">{data?.sources.map((source) => <article key={`${source.sourceId}/${source.digest}`} aria-label={`Shared workspace ${source.name}`}><div><h3>{source.name}</h3><p>{source.pages} pages · {source.tasks} tasks · {source.lessons} lessons · {source.groups} terminal groups</p><p>Shared through {source.hosts.join(", ") || "connected machines"} · Revision {source.revision}</p><small>Snapshot <code>{source.digest.slice(0, 12)}</code> · Updated {Number.isFinite(Date.parse(source.updatedAt)) ? new Date(source.updatedAt).toLocaleString() : "at an unknown time"}</small>{source.conflict && <p role="status" className="peer-sync-warning">{source.conflict}</p>}</div><button className="button secondary compact" disabled={!fresh || reading || busy || Boolean(intent)} onClick={() => setSelected(source)}><Copy size={14} /> Import a copy</button></article>)}</div>
    {selected && <Confirm title={`Import ${selected.name}?`} description={`${selected.conflict ? `${selected.conflict} ` : ""}Copy snapshot ${selected.digest.slice(0, 12)} into this Relay workspace as separate pages, tasks, lessons, and mapped terminal groups. Existing content is preserved. This does not start processes, copy authentication, or take terminal control.`} action="Import a separate copy" onClose={() => { if (!busy) setSelected(undefined); }} onConfirm={importSource} />}
  </section>;
}
