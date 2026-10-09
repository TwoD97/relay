import { useEffect, useRef, useState, type FormEvent } from "react";
import { ArrowRight, Check, CircleAlert, KeyRound, LoaderCircle, ShieldCheck, Terminal as TerminalIcon } from "lucide-react";
import { api, errorMessage, hostPath } from "./api";
import { Modal } from "./Modal";
import { FolderPicker } from "./FolderPicker";
import { Terminal } from "./LazyTerminal";
import type { Harness, HarnessId, Host, Session } from "./types";

export function InlineError({ error }: { error: string | null | undefined }) {
  return error ? <div role="alert" className="inline-error"><CircleAlert size={16} /><span>{error}</span></div> : null;
}

export function AddHost({ onClose, onCreated }: { onClose: () => void; onCreated: (host: Host) => void }) {
  const [name, setName] = useState("");
  const [target, setTarget] = useState("");
  const [port, setPort] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (busy) return;
    setBusy(true); setError(null);
    try { onCreated(await api.addHost({ name: name.trim() || target.trim(), target: target.trim(), port: Number(port) })); }
    catch (error) { setError(errorMessage(error)); setBusy(false); }
  };
  return <Modal title="Connect a machine" subtitle="One SSH connection. A workspace that stays with you." onClose={onClose}>
    <form onSubmit={(event) => void submit(event)} className="modal-body form-stack">
      <div className="connection-preview"><div className="connection-node"><TerminalIcon size={20} /><span>Relay</span></div><div className="connection-line"><span>SSH</span></div><div className="connection-node remote"><KeyRound size={20} /><span>Your machine</span></div></div>
      <label className="field"><span>SSH destination <span className="required">*</span></span><input data-autofocus="true" required autoCapitalize="none" autoCorrect="off" spellCheck={false} autoComplete="off" value={target} onChange={(event) => setTarget(event.target.value)} placeholder="you@your-server or an SSH alias" /><small>Use an address or a host from your local SSH config.</small></label>
      <div className="form-columns"><label className="field"><span>Display name</span><input maxLength={80} value={name} onChange={(event) => setName(event.target.value)} placeholder="Development" autoComplete="off" /></label><label className="field port-field"><span>Port override (optional)</span><input type="number" min={1} max={65535} value={port} placeholder="SSH config or 22" onChange={(event) => setPort(event.target.value)} inputMode="numeric" /></label></div>
      <div className="help-box"><ShieldCheck size={18} /><p>Use your SSH key or sign in in the next step. Relay installs its runtime in your home folder and keeps sessions running when you disconnect.</p></div>
      <InlineError error={error} />
      <div className="modal-actions"><button type="button" className="button secondary" onClick={onClose}>Cancel</button><button className="button primary" type="submit" disabled={busy || !target.trim()}>{busy ? <LoaderCircle className="spin" size={16} /> : <ArrowRight size={16} />}{busy ? "Connecting…" : "Connect machine"}</button></div>
    </form>
  </Modal>;
}

export function NewSession({ host, harnesses, existing, onClose, onCreated }: { host: Host; harnesses: Harness[]; existing: Session[]; onClose: () => void; onCreated: (session: Session, notice?: string) => void }) {
  const mounted = useRef(true);
  const submitting = useRef(false);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const latest = [...existing].sort((a, b) => String(b.createdAt).localeCompare(String(a.createdAt)))[0];
  const [title, setTitle] = useState("");
  const [workspace, setWorkspace] = useState(latest?.workspace || "My workspace");
  const [cwd, setCwd] = useState(latest?.cwd || "");
  const [harness, setHarness] = useState<HarnessId>("shell");
  const [sharedContext, setSharedContext] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const options: { id: HarnessId; name: string; description: string }[] = [{ id: "shell", name: "Terminal", description: "Your remote shell" }, { id: "claude", name: "Claude Code", description: "Anthropic" }, { id: "codex", name: "Codex", description: "OpenAI" }];
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (submitting.current) return;
    submitting.current = true;
    setBusy(true); setError(null);
    try {
      let notice: string | undefined;
      if (sharedContext && harness !== "shell") {
        const prepared = await api.prepareProjectContext(host.id, cwd);
        notice = ["Shared project context is ready.", ...(prepared.warnings ?? [])].join(" ");
      }
      if (!mounted.current) return;
      const created = await api.createSession(host.id, { title: title.trim() || options.find((item) => item.id === harness)!.name, workspace: workspace.trim(), cwd, harness });
      if (mounted.current) onCreated(created, notice);
    }
    catch (error) { setError(errorMessage(error)); setBusy(false); }
    finally { submitting.current = false; }
  };
  return <Modal title="Start a session" subtitle={`A fresh terminal on ${host.name}.`} onClose={onClose}>
    <form className="modal-body form-stack" onSubmit={(event) => void submit(event)}>
      <fieldset className="harness-picker" disabled={busy}><legend>Run</legend><div className="harness-options">{options.map((option) => {
        const available = option.id === "shell" || harnesses.some((item) => item.id === option.id && item.installed);
        return <label key={option.id} className={`harness-option ${harness === option.id ? "selected" : ""} ${!available ? "unavailable" : ""}`}><input type="radio" name="harness" value={option.id} checked={harness === option.id} disabled={!available} onChange={() => setHarness(option.id)} /><TerminalIcon size={18} /><strong>{option.name}</strong><small>{available ? option.description : "Install first"}</small></label>;
      })}</div></fieldset>
      <FolderPicker key={host.id} host={host} value={cwd} onChange={setCwd} disabled={busy} />
      {harness !== "shell" && <label className="shared-context-option"><input type="checkbox" checked={sharedContext} disabled={busy} onChange={(event) => setSharedContext(event.target.checked)} /><span><strong>Shared instructions and memory</strong><small>Connect both agents to AGENTS.md, MEMORY.md and HANDOFF.md in this folder. Existing content is kept. Native chat histories stay separate.</small></span></label>}
      <label className="field"><span>Workspace <span className="required">*</span></span><input disabled={busy} required maxLength={80} list="workspace-names" value={workspace} onChange={(event) => setWorkspace(event.target.value)} /><datalist id="workspace-names">{[...new Set(existing.map((session) => session.workspace))].map((name) => <option key={name} value={name} />)}</datalist><small>Group related sessions together in the sidebar.</small></label>
      <label className="field"><span>Session name</span><input disabled={busy} maxLength={120} value={title} onChange={(event) => setTitle(event.target.value)} placeholder="What are you working on?" /></label>
      <InlineError error={error} />
      <div className="modal-actions"><button type="button" className="button secondary" onClick={onClose}>Cancel</button><button className="button primary" type="submit" disabled={busy || !cwd.trim() || !workspace.trim() || host.status !== "online"}>{busy ? <LoaderCircle className="spin" size={16} /> : <TerminalIcon size={16} />}{busy ? "Starting…" : "Start session"}</button></div>
    </form>
  </Modal>;
}

export function SetupHost({ host, onClose, onOpen, onRetry }: { host: Host; onClose: () => void; onOpen: () => void; onRetry: () => void }) {
  const ready = host.status === "online";
  return <Modal title={ready ? `${host.name} is ready` : `Connecting to ${host.name}`} subtitle={`${host.target} · ${host.port ? `Port ${host.port}` : "SSH config defaults"}`} onClose={onClose} wide>
    <div className="setup-body">
      <div className={`setup-stage ${ready ? "complete" : ""}`}>{ready ? <Check size={18} /> : host.status === "error" ? <CircleAlert size={18} /> : <LoaderCircle className="spin" size={18} />}<div><strong>{host.stage || (ready ? "Connected" : "Starting SSH connection")}</strong><p>{ready ? "Your remote runtime is available. You can start a session or set up a coding agent." : "Follow the SSH prompts below. Verify a new host’s fingerprint before trusting it."}</p></div></div>
      <InlineError error={host.error} />
      <div className="setup-terminal"><Terminal path={hostPath(host.id, "setup-terminal")} enabled={!ready && host.status !== "disconnected" && host.status !== "error"} autoReconnect exclusive={false} label="SSH setup terminal" /></div>
      <div className="setup-footer"><span><ShieldCheck size={14} /> SSH handles your credentials</span>{ready ? <button className="button primary" onClick={onOpen}>Open machine <ArrowRight size={16} /></button> : host.status === "error" || host.status === "disconnected" ? <button className="button primary" onClick={onRetry}>Retry connection</button> : <button className="button secondary" onClick={onClose}>Continue in background</button>}</div>
    </div>
  </Modal>;
}

export function Confirm({ title, description, action, onClose, onConfirm }: { title: string; description: string; action: string; onClose: () => void; onConfirm: () => Promise<void> }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  return <Modal title={title} onClose={onClose}><div className="modal-body form-stack"><p className="confirm-description">{description}</p><InlineError error={error} /><div className="modal-actions"><button className="button secondary" onClick={onClose}>Cancel</button><button className="button danger" disabled={busy} onClick={async () => { setBusy(true); setError(null); try { await onConfirm(); onClose(); } catch (error) { setError(errorMessage(error)); setBusy(false); } }}>{busy && <LoaderCircle className="spin" size={15} />}{action}</button></div></div></Modal>;
}
