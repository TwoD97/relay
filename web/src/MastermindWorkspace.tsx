import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { ArrowUpRight, BrainCircuit, CircleAlert, FileCode2, FilePlus2, ListTodo, LoaderCircle, Pencil, Plus, RefreshCw, Trash2 } from "lucide-react";
import { api, errorMessage } from "./api";
import { useAppearance } from "./appearance";
import { Confirm, InlineError } from "./Forms";
import { Mastermind } from "./Mastermind";
import { mastermindHTML } from "./mastermindHtml";
import { Modal } from "./Modal";
import { useMastermindWorkspace } from "./useMastermindWorkspace";
import type { MastermindActions } from "./useMastermind";
import type { FleetState, Host, MastermindPage, MastermindTask, MastermindTaskStatus, Session } from "./types";
import "./MastermindWorkspace.css";

const statuses: { value: MastermindTaskStatus; label: string }[] = [{ value: "todo", label: "To do" }, { value: "doing", label: "In progress" }, { value: "blocked", label: "Blocked" }, { value: "done", label: "Done" }];
type Props = { fleet: FleetState; actions: MastermindActions; pageId?: string; expanded: boolean; onPage: (id: string | undefined) => void; onExpand: () => void; onAdd: () => void; onHost: (host: Host) => void; onSession: (host: Host, session: Session) => void; onActivity: () => void };

export function MastermindWorkspace({ fleet, actions, pageId, expanded, onPage, onExpand, ...navigation }: Props) {
  const workspace = useMastermindWorkspace();
  const { state } = workspace;
  const appearance = useAppearance();
  const [page, setPage] = useState<MastermindPage>();
  const [pageError, setPageError] = useState<string>();
  const editorGeneration = useRef(0);
  const [pageReload, setPageReload] = useState(0);
  const [loadingPage, setLoadingPage] = useState(false);
  const [pageEditor, setPageEditor] = useState<{ revision: number; page?: MastermindPage }>();
  const [taskEditor, setTaskEditor] = useState<{ revision: number; task?: MastermindTask }>();
  const [reviewing, setReviewing] = useState(false);
  const [removing, setRemoving] = useState<{ revision: number; kind: "page" | "task"; id: string; title: string }>();
  const selected = pageId ? state?.pages.find((item) => item.id === pageId) : state?.pages[0];
  const locked = !state || workspace.busy || workspace.uncertain || Boolean(workspace.error || fleet.error);
  useEffect(() => {
    setPageError(undefined);
    if (!selected) { setPage(undefined); setLoadingPage(false); return; }
    const controller = new AbortController();
    let current = true;
    setLoadingPage(true);
    void api.mastermindPage(selected.id, controller.signal).then((value) => {
      if (value.id !== selected.id || typeof value.html !== "string" || !Number.isSafeInteger(value.revision)) throw new Error("The canvas page identity could not be verified.");
      if (current) setPage(value);
    }).catch((cause) => { if (current) setPageError(errorMessage(cause)); }).finally(() => { if (current) setLoadingPage(false); });
    return () => { current = false; controller.abort(); };
  }, [selected?.id, selected?.updatedAt, pageReload]);
  const document = useMemo(() => {
    if (!page || page.id !== selected?.id || loadingPage || pageError) return undefined;
    try { return { html: mastermindHTML(page.html, appearance.resolved) }; }
    catch (cause) { return { error: errorMessage(cause) }; }
  }, [page, selected?.id, loadingPage, pageError, appearance.resolved]);
  const updateStatus = (task: MastermindTask, status: MastermindTaskStatus) => {
    if (!state || locked) return;
    void workspace.mutate(state.revision, () => api.saveMastermindTask(state.revision, { ...task, status }, task.id), { kind: "task", id: task.id });
  };
  const beginPage = () => { if (state) { workspace.clearMutationError(); editorGeneration.current++; setPageEditor({ revision: state.revision }); } };
  const beginTask = () => { if (state) { workspace.clearMutationError(); editorGeneration.current++; setTaskEditor({ revision: state.revision }); } };
  return <div className={`mastermind-workspace ${expanded ? "mastermind-expanded" : ""}`}>
    <header className="workspace-heading"><div><span className="workspace-eyebrow"><BrainCircuit size={14} /> YOUR COORDINATOR</span><h1>Mastermind workspace</h1><p>A shared plan and canvas, maintained by you and Mastermind.</p></div><button className="icon-button" aria-label="Refresh Mastermind workspace" title="Refresh workspace" onClick={() => void workspace.refresh()}><RefreshCw size={16} /></button></header>
    {(workspace.error || workspace.mutationError || workspace.uncertain) && <div className="workspace-errors"><InlineError error={workspace.error || workspace.mutationError} />{workspace.uncertain && <><p>The previous change is unconfirmed. Review the current workspace; Relay will not repeat it.</p>{workspace.unknown?.id && <p>Requested {workspace.unknown.kind ?? "item"} ID: <code>{workspace.unknown.id}</code></p>}<button className="text-button" disabled={!workspace.canResume || Boolean(fleet.error)} onClick={() => setReviewing(true)}>Review and resume editing</button></>}</div>}
    <div className="workspace-content">
      <section className="workspace-tasks" aria-label="Mastermind tasks"><header><h2><ListTodo size={17} />Tasks <span>{state?.tasks.length ?? "—"}</span></h2><button className="icon-button" aria-label="New task" title="New task" onClick={beginTask} disabled={locked || (state?.tasks.length ?? 0) >= 200}><Plus size={16} /></button></header>
        <div className="workspace-task-list">{!state ? <p className="workspace-empty">{workspace.error ? "Tasks could not be loaded." : "Loading tasks…"}</p> : !state.tasks.length ? <div className="workspace-empty"><ListTodo size={25} /><h3>Make a plan together</h3><p>Ask Mastermind to plan your work, or add the first task.</p><button className="text-button" disabled={locked} onClick={beginTask}>Add a task <Plus size={13} /></button></div> : state.tasks.map((task) => {
          const host = fleet.hosts.find((item) => item.id === task.hostId);
          const session = host && fleet.runtimes[host.id]?.sessions.find((item) => item.id === task.sessionId);
          return <article className={`workspace-task task-${task.status}`} key={task.id} aria-label={task.title}><div className="workspace-task-title"><strong>{task.title}</strong><button className="icon-button" aria-label={`Edit task ${task.title}`} disabled={locked} onClick={() => { editorGeneration.current++; setTaskEditor({ revision: state.revision, task }); }}><Pencil size={13} /></button></div>{task.detail && <p>{task.detail}</p>}<label className="task-status"><span className="sr-only">Status for {task.title}</span><select aria-label={`Status for ${task.title}`} value={task.status} disabled={locked} onChange={(event) => updateStatus(task, event.target.value as MastermindTaskStatus)}>{statuses.map((status) => <option key={status.value} value={status.value}>{status.label}</option>)}</select></label><div className="task-links">{task.pageId && state.pages.some((item) => item.id === task.pageId) && <button className="text-button" onClick={() => onPage(task.pageId)}>Open page <ArrowUpRight size={12} /></button>}{host && session && <button className="text-button" onClick={() => navigation.onSession(host, session)}>{host.name} · Session <ArrowUpRight size={12} /></button>}{host && !session && <button className="text-button" onClick={() => navigation.onHost(host)}>{host.name} <ArrowUpRight size={12} /></button>}</div></article>;
        })}</div>
      </section>
      <section className="workspace-canvas" aria-label="Mastermind canvas">
        <header className="canvas-toolbar"><div className="canvas-tabs" role="tablist" aria-label="Canvas pages">{state?.pages.map((item) => <button key={item.id} role="tab" tabIndex={selected?.id === item.id ? 0 : -1} onKeyDown={(event) => { if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return; event.preventDefault(); const tabs = Array.from(event.currentTarget.parentElement!.querySelectorAll<HTMLButtonElement>("[role=tab]")); const index = tabs.indexOf(event.currentTarget); const next = event.key === "Home" ? 0 : event.key === "End" ? tabs.length - 1 : (index + (event.key === "ArrowRight" ? 1 : -1) + tabs.length) % tabs.length; tabs[next]?.focus(); tabs[next]?.click(); }} aria-selected={selected?.id === item.id} aria-controls="mastermind-canvas-panel" id={`canvas-tab-${item.id}`} onClick={() => onPage(item.id)}><FileCode2 size={14} /><span>{item.title}</span></button>)}</div><button className="icon-button" aria-label="New page" title="New page" onClick={beginPage} disabled={locked || (state?.pages.length ?? 0) >= 16}><FilePlus2 size={16} /></button></header>
        {selected && <div className="canvas-page-actions"><span>{selected.title}</span><button className="text-button" disabled={locked || loadingPage || !page || page.id !== selected.id || Boolean(pageError)} onClick={() => { editorGeneration.current++; setPageEditor({ revision: state!.revision, page }); }}><Pencil size={12} />Edit HTML</button><button className="icon-button" aria-label="Delete canvas page" title="Delete canvas page" disabled={locked} onClick={() => setRemoving({ revision: state!.revision, kind: "page", id: selected.id, title: selected.title })}><Trash2 size={13} /></button></div>}
        <div className="canvas-document" id="mastermind-canvas-panel" role="tabpanel" aria-labelledby={selected ? `canvas-tab-${selected.id}` : undefined}>
          {loadingPage ? <div className="workspace-empty"><LoaderCircle className="spin" size={23} /><p>Loading canvas page…</p></div> : pageError || document?.error ? <div className="workspace-empty"><CircleAlert size={23} /><p>{pageError || document?.error}</p>{pageError && <button className="text-button" onClick={() => setPageReload((value) => value + 1)}>Reload page</button>}</div> : document?.html ? <iframe title={`${selected!.title} canvas`} sandbox="" referrerPolicy="no-referrer" srcDoc={document.html} /> : <div className="workspace-empty"><FileCode2 size={30} /><h3>{pageId && !selected ? "This page is unavailable" : "A canvas for the bigger picture"}</h3><p>{pageId && !selected ? "It may have been removed. Choose another page or create a new one." : "Mastermind can publish plans, notes, reports, and diagrams here. Pages render safely without scripts or external connections."}</p><button className="button secondary compact" disabled={locked} onClick={beginPage}><FilePlus2 size={14} />Create a page</button></div>}
        </div>
      </section>
    </div>
    <Mastermind fleet={fleet} actions={actions} {...navigation} expanded={expanded} onExpand={onExpand} />
    {pageEditor && state && <PageEditor initial={pageEditor.page} revision={pageEditor.revision} currentRevision={state.revision} busy={workspace.busy} locked={locked} error={workspace.mutationError} onClose={() => { editorGeneration.current++; setPageEditor(undefined); }} onSave={async (value) => {
      const generation = editorGeneration.current;
      const route = location.hash;
      const id = pageEditor.page?.id ?? crypto.randomUUID().replaceAll("-", "");
      const result = await workspace.mutate(pageEditor.revision, () => api.saveMastermindPage(pageEditor.revision, value, pageEditor.page?.id, id), { kind: "page", id });
      if (result && generation === editorGeneration.current && location.hash === route) { setPageEditor(undefined); if (result.page) { setPage(result.page); onPage(result.page.id); } }
    }} />}
    {taskEditor && state && <TaskEditor initial={taskEditor.task} fleet={fleet} pages={state.pages} revision={taskEditor.revision} currentRevision={state.revision} busy={workspace.busy} locked={locked} error={workspace.mutationError} onClose={() => { editorGeneration.current++; setTaskEditor(undefined); }} onDelete={() => { const task = taskEditor.task!; setTaskEditor(undefined); setRemoving({ revision: state.revision, kind: "task", id: task.id, title: task.title }); }} onSave={async (value) => {
      const generation = editorGeneration.current;
      const id = taskEditor.task?.id ?? crypto.randomUUID().replaceAll("-", "");
      const result = await workspace.mutate(taskEditor.revision, () => api.saveMastermindTask(taskEditor.revision, value, taskEditor.task?.id, id), { kind: "task", id });
      if (result && generation === editorGeneration.current) setTaskEditor(undefined);
    }} />}
    {reviewing && <Confirm title="Resume workspace editing?" description="Review the current pages and tasks before continuing. The previous write may still complete; its outcome remains unknown. Resuming does not repeat it. New changes use the current revision so concurrent edits cannot silently overwrite one another." action="Resume editing" onClose={() => setReviewing(false)} onConfirm={async () => { if (fleet.error || !workspace.resumeEditing()) throw new Error("Read the current workspace successfully before resuming editing."); }} />}
    {removing && <Confirm title={`Delete “${removing.title}”?`} description={removing.kind === "page" ? "This removes the canvas page. Tasks and terminal sessions are kept." : "This removes the task from your shared plan. Linked sessions keep running."} action={removing.kind === "page" ? "Delete page" : "Delete task"} onClose={() => setRemoving(undefined)} onConfirm={async () => {
      const result = await workspace.mutate(removing.revision, () => removing.kind === "page" ? api.removeMastermindPage(removing.revision, removing.id) : api.removeMastermindTask(removing.revision, removing.id), { kind: removing.kind, id: removing.id });
      if (!result) throw new Error("Deletion was not confirmed. Review the current workspace before another action.");
      if (removing.kind === "page" && selected?.id === removing.id) onPage(undefined);
    }} />}
  </div>;
}

type EditorProps = { revision: number; currentRevision: number; busy: boolean; locked: boolean; error?: string; onClose: () => void };
function PageEditor({ initial, revision, currentRevision, busy, locked, error, onClose, onSave }: EditorProps & { initial?: MastermindPage; onSave: (value: { title: string; html: string }) => Promise<void> }) {
  const [title, setTitle] = useState(initial?.title ?? "");
  const [html, setHTML] = useState(initial?.html ?? "<h1>Project plan</h1>\n<p>Describe the goal and next steps.</p>");
  const oversized = new TextEncoder().encode(html).length > 128 * 1024;
  const disabled = locked || !title.trim() || oversized || revision !== currentRevision;
  return <Modal wide title={initial ? "Edit canvas page" : "New canvas page"} subtitle="HTML and inline styles are supported. Scripts, forms, navigation, and external assets are disabled." onClose={onClose}><form className="modal-body form-stack" onSubmit={(event) => { event.preventDefault(); if (!disabled) void onSave({ title, html }); }}><label className="field"><span>Page title</span><input value={title} maxLength={160} onChange={(event) => setTitle(event.target.value)} disabled={busy} required autoFocus /></label><label className="field"><span>Page HTML</span><textarea aria-label="Page HTML" className="canvas-html-editor" value={html} onChange={(event) => setHTML(event.target.value)} disabled={busy} spellCheck={false} rows={12} /></label>{oversized && <InlineError error="Canvas pages must be no larger than 128 KiB." />}{revision !== currentRevision && <InlineError error="The workspace changed while this editor was open. Keep your draft, then reopen the editor to review current content." />}<InlineError error={error} /><div className="modal-actions"><button type="button" className="button secondary" onClick={onClose}>Cancel</button><button type="submit" className="button primary" disabled={disabled}>{busy ? "Saving…" : "Save page"}</button></div></form></Modal>;
}

type TaskValue = Pick<MastermindTask, "title" | "detail" | "status" | "hostId" | "sessionId" | "pageId">;
function TaskEditor({ initial, fleet, pages, revision, currentRevision, busy, locked, error, onClose, onDelete, onSave }: EditorProps & { initial?: MastermindTask; fleet: FleetState; pages: { id: string; title: string }[]; onDelete: () => void; onSave: (value: TaskValue) => Promise<void> }) {
  const [value, setValue] = useState<TaskValue>({ title: initial?.title ?? "", detail: initial?.detail ?? "", status: initial?.status ?? "todo", hostId: initial?.hostId, sessionId: initial?.sessionId, pageId: initial?.pageId });
  const oversized = new TextEncoder().encode(value.detail).length > 4096;
  const disabled = locked || !value.title.trim() || oversized || revision !== currentRevision;
  const submit = (event: FormEvent) => { event.preventDefault(); if (!disabled) void onSave(value); };
  const sessions = value.hostId ? fleet.runtimes[value.hostId]?.sessions ?? [] : [];
  return <Modal title={initial ? "Edit task" : "New task"} subtitle="A task in your shared plan. Changing its status never sends terminal input." onClose={onClose}><form className="modal-body form-stack" onSubmit={submit}><label className="field"><span>Task title</span><input value={value.title} maxLength={160} onChange={(event) => setValue({ ...value, title: event.target.value })} disabled={busy} required autoFocus /></label><label className="field"><span>Details</span><textarea aria-label="Details" value={value.detail} maxLength={4096} onChange={(event) => setValue({ ...value, detail: event.target.value })} disabled={busy} rows={4} /></label><label className="field"><span>Task status</span><select aria-label="Task status" value={value.status} onChange={(event) => setValue({ ...value, status: event.target.value as MastermindTaskStatus })} disabled={busy}>{statuses.map((status) => <option key={status.value} value={status.value}>{status.label}</option>)}</select></label><div className="form-two-columns"><label className="field"><span>Canvas page</span><select aria-label="Canvas page" value={value.pageId ?? ""} onChange={(event) => setValue({ ...value, pageId: event.target.value || undefined })} disabled={busy}><option value="">No linked page</option>{pages.map((page) => <option key={page.id} value={page.id}>{page.title}</option>)}</select></label><label className="field"><span>Linked machine</span><select aria-label="Linked machine" value={value.hostId ?? ""} onChange={(event) => setValue({ ...value, hostId: event.target.value || undefined, sessionId: undefined })} disabled={busy}><option value="">No linked machine</option>{value.hostId && !fleet.hosts.some((host) => host.id === value.hostId) && <option value={value.hostId}>Machine unavailable</option>}{fleet.hosts.map((host) => <option key={host.id} value={host.id}>{host.name}</option>)}</select></label></div>{value.hostId && <label className="field"><span>Linked session</span><select aria-label="Linked session" value={value.sessionId ?? ""} onChange={(event) => setValue({ ...value, sessionId: event.target.value || undefined })} disabled={busy}><option value="">No linked session</option>{value.sessionId && !sessions.some((session) => session.id === value.sessionId) && <option value={value.sessionId}>Session unavailable</option>}{sessions.map((session) => <option key={session.id} value={session.id}>{session.title}</option>)}</select></label>}{oversized && <InlineError error="Task details must be no larger than 4 KiB." />}{revision !== currentRevision && <InlineError error="The workspace changed. Reopen this task to review its current state." />}<InlineError error={error} /><div className="modal-actions">{initial && <button className="button ghost-danger" type="button" disabled={locked} onClick={onDelete}>Delete task</button>}<button type="button" className="button secondary" onClick={onClose}>Cancel</button><button type="submit" className="button primary" disabled={disabled}>{busy ? "Saving…" : "Save task"}</button></div></form></Modal>;
}
