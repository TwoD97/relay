import { useEffect, useRef, useState, type FormEvent } from "react";
import { ArrowUpRight, BrainCircuit, CircleAlert, Folder, LoaderCircle, Play, RefreshCw, Server, Settings2, Terminal as TerminalIcon } from "lucide-react";
import { runtimePath } from "./api";
import { AgentModeBadge, AgentModePicker } from "./AgentMode";
import { SessionApprovalNotice } from "./Approvals";
import { FolderPicker } from "./FolderPicker";
import { InlineError } from "./Forms";
import { Terminal } from "./LazyTerminal";
import { Modal } from "./Modal";
import type { AgentMode, FleetState, Host, MastermindState, Session } from "./types";
import type { MastermindActions } from "./useMastermind";
import "./Mastermind.css";

const provider = (id: string) => id === "claude" ? "Claude Code" : "Codex";
const statusLabels: Record<MastermindState["status"], string> = {
  unconfigured: "Not set up", disabled: "Paused", waiting: "Waiting for machine", starting: "Starting…",
  running: "Session running", exited: "Session finished", interrupted: "Session interrupted",
  missing: "Session unavailable", blocked: "Needs attention", uncertain: "Checking previous start",
};

type Props = { fleet: FleetState; actions: MastermindActions; onAdd: () => void; onHost: (host: Host) => void; onSession: (host: Host, session: Session) => void; onActivity: () => void };

export function Mastermind({ fleet, actions, onAdd, onHost, onSession, onActivity }: Props) {
  const [settings, setSettings] = useState(false);
  const state = actions.state;
  const host = fleet.hosts.find((item) => item.id === state?.config?.hostId);
  const runtime = host && fleet.runtimes[host.id];
  const saved = state?.session;
  const worker = state?.execution === "worker";
  const identitySupported = saved && (worker ? saved.harness === "shell" && !saved.mastermindId && !saved.agentMode : (state?.execution === undefined || state.execution === "native") && Boolean(saved.mastermindId) && saved.harness !== "shell");
  const session = saved && identitySupported ? runtime?.sessions.find((item) => item.id === saved.id && item.createdAt === saved.createdAt && (worker ? !item.mastermindId && !item.agentMode : item.mastermindId === saved.mastermindId && item.agentMode === saved.agentMode) && item.harness === saved.harness && item.cwd === saved.cwd && item.title === saved.title && item.workspace === saved.workspace && item.purpose === saved.purpose) : undefined;
  const current = Boolean(!fleet.error && host?.status === "online" && runtime?.fetchedAt && !runtime.sessionsError);
  const confirmed = Boolean(state && ["running", "exited", "interrupted"].includes(state.status));
  const verified = Boolean(confirmed && current && session);
  const unavailable = Boolean(saved && !verified);
  const locked = actions.busy || actions.uncertain || Boolean(fleet.error);
  const openSettings = () => { actions.clearError(); setSettings(true); };
  const startAvailable = state && ((!state.enabled && state.config) || ["exited", "interrupted", "missing"].includes(state.status));
  const setupCheck = state?.status === "blocked" && state.enabled && ["runtime", "harness", "folder", "worker"].includes(state.blocker ?? "");
  const harness = runtime?.harnesses.find((item) => item.id === state?.config?.harness);
  const label = fleet.error ? "Connection unverified" : !state ? fleet.loading ? "Loading…" : "Unavailable" : saved && host?.status !== "online" ? "Machine offline" : saved && current && !session ? "Session unverified" : statusLabels[state.status];

  return <section className="mastermind" aria-label="Mastermind">
    <header className="mastermind-heading">
      <span className="mastermind-icon"><BrainCircuit size={21} /></span>
      <div><div className="mastermind-title"><h2>Mastermind</h2><span className={`mastermind-state ${state?.enabled && verified && session?.status === "running" ? "running" : state && ["blocked", "uncertain", "interrupted", "missing"].includes(state.status) ? "attention" : ""}`}>{label}</span></div><p>Your persistent agent for planning and coordinating work across machines.</p></div>
      {state?.config && <nav className="mastermind-actions" aria-label="Mastermind actions">
        {state.enabled && verified && host && session && <button className="button secondary compact" onClick={() => onSession(host, session)}><ArrowUpRight size={14} />Open full terminal</button>}
        <button className="icon-button" aria-label="Mastermind settings" title="Mastermind settings" disabled={actions.busy} onClick={openSettings}><Settings2 size={17} /></button>
      </nav>}
    </header>

    {state?.config ? <>
      <div className="mastermind-context"><span><Server size={13} />{host?.name ?? "Machine unavailable"}</span><span>{provider(state.config.harness)}</span><span className="mono"><Folder size={13} />{saved?.cwd ?? state.config.cwd}</span>{saved && !worker ? <AgentModeBadge session={saved} /> : <span className={`agent-mode-badge ${state.config.agentMode === "auto" ? "mode-auto" : ""}`} aria-label={`Launch mode: ${state.config.agentMode === "auto" ? "Auto · full access" : "Manual"}`} title={worker && verified ? "Coordinator launch mode verified by Relay. Existing account and project restrictions still apply." : "Configured launch mode"}>{state.config.agentMode === "auto" ? "Auto · full access" : "Manual"}</span>}</div>
      {worker && saved && <p className="mastermind-compatibility">Mastermind uses a compatible session on this runtime. Your other sessions stay running.{state.config.agentMode === "manual" && " Review any permission prompts in its terminal; approval cards are unavailable for this session."}</p>}
      {(state.status !== "running" || state.error || unavailable || !state.enabled) && <div className="mastermind-notice">{["starting", "waiting"].includes(state.status) ? <LoaderCircle className="spin" size={15} /> : <CircleAlert size={15} />}<div>
        <p>{!state.enabled ? "Automatic Mastermind connection is paused. Its session and output are kept." : unavailable ? !current ? "Reconnect this machine to verify and resume the existing Mastermind session." : !session ? "The saved Mastermind does not match a current session. Its terminal cannot be attached." : "Waiting for the controller to verify this Mastermind session before attaching its terminal." : state.stage}</p>
        {unavailable && current && session && <p>{state.stage}</p>}
        {state.error && <p>{state.error}</p>}
        {state.status === "uncertain" && <p>Relay is checking the previous start. It will not launch another coordinator while the outcome is unknown.</p>}
        {host && !current && state.status !== "blocked" && <button className="text-button" onClick={() => onHost(host)}>Open machine <ArrowUpRight size={12} /></button>}
        {state.status === "blocked" && <div className="mastermind-setup-actions">
          {state.blocker === "harness" && <p>{!harness?.installed ? `Install ${provider(state.config.harness)} on this machine, then check setup.` : harness.authenticated !== true ? `Verify ${provider(state.config.harness)} sign-in on this machine, then check setup.` : `Update or repair ${provider(state.config.harness)} on this machine, then check setup.`}</p>}
          {state.blocker === "folder" && <button className="text-button" disabled={actions.busy} onClick={openSettings}>Choose a working folder <ArrowUpRight size={12} /></button>}
          {host && state.blocker !== "folder" && state.blocker !== "storage" && <button className="text-button" onClick={() => onHost(host)}>{state.blocker === "harness" ? harness?.installed && harness.authenticated !== true ? `Sign in to ${provider(state.config.harness)}` : `Set up ${provider(state.config.harness)}` : state.blocker === "identity" ? "Inspect machine sessions" : ["runtime", "worker"].includes(state.blocker ?? "") ? "Open runtime setup" : "Open machine"} <ArrowUpRight size={12} /></button>}
          {(state.blocker === "launch" || !host || !state.blocker) && <button className="text-button" disabled={actions.busy} onClick={openSettings}>Review Mastermind settings <ArrowUpRight size={12} /></button>}
          {setupCheck && <button className="button secondary compact" disabled={locked || host?.status !== "online"} onClick={() => void actions.start(state.revision)}>{actions.busy ? <LoaderCircle className="spin" size={14} /> : <RefreshCw size={14} />}{actions.busy ? "Checking…" : "Check setup"}</button>}
          {state.blocker === "launch" && <button className="button secondary compact" disabled={locked || host?.status !== "online"} onClick={() => void actions.start(state.revision)}>{actions.busy ? <LoaderCircle className="spin" size={14} /> : <Play size={14} />}{actions.busy ? "Starting…" : "Retry start"}</button>}
        </div>}
      </div></div>}
      {state.enabled && verified && host && session && <div className="mastermind-terminal"><Terminal key={JSON.stringify([host.id, session.id, session.createdAt, session.mastermindId])} path={runtimePath(host.id, `sessions/${encodeURIComponent(session.id)}/terminal`)} finished={session.status !== "running"} label="Mastermind terminal" /></div>}
      {state.enabled && verified && host && session && !worker && <SessionApprovalNotice host={host} session={session} runtime={runtime} onActivity={onActivity} />}
      {startAvailable && <div className="mastermind-footer"><p className="mastermind-note">{!state.enabled ? "Resume reconnects an existing session when it is still running." : "Start a new coordinator when you are ready. Previous session output is preserved."}</p><button className="button secondary compact" disabled={locked || host?.status !== "online"} onClick={() => void actions.start(state.revision)}>{actions.busy ? <LoaderCircle className="spin" size={14} /> : <Play size={14} />}{!state.enabled ? "Resume Mastermind" : "Start Mastermind"}</button></div>}
    </> : <div className="mastermind-empty"><div><h3>{!state && !fleet.loading ? "Mastermind state is unavailable" : "Choose the agent at the center of your fleet"}</h3><p>{!state && !fleet.loading ? "Reconnect Relay or update its local controller to load Mastermind settings." : "Pick a machine, coding agent, and project folder. Your coordinator stays available here when you return."}</p></div>{state && <button className="button primary" onClick={openSettings} disabled={locked}><BrainCircuit size={16} />Set up Mastermind</button>}</div>}
    {!settings && <InlineError error={actions.error} />}
    {!state?.config && <InlineError error={state?.error} />}
    {actions.uncertain && <div className="mastermind-notice"><CircleAlert size={15} /><div><p>The previous request may have completed. Relay has not repeated it.</p><button className="text-button" disabled={actions.checking} onClick={() => void actions.check()}>{actions.checking ? "Checking…" : "Check Mastermind status"}</button></div></div>}
    {settings && state && <MastermindSettings initial={state} fleet={fleet} actions={actions} onClose={() => setSettings(false)} onAdd={() => { setSettings(false); onAdd(); }} onHost={(target) => { setSettings(false); onHost(target); }} />}
  </section>;
}

function MastermindSettings({ initial, fleet, actions, onClose, onAdd, onHost }: { initial: MastermindState; fleet: FleetState; actions: MastermindActions; onClose: () => void; onAdd: () => void; onHost: (host: Host) => void }) {
  const [revision] = useState(initial.revision);
  const [hostId, setHostId] = useState(initial.config?.hostId ?? fleet.hosts.find((host) => host.status === "online")?.id ?? fleet.hosts[0]?.id ?? "");
  const [harness, setHarness] = useState<"claude" | "codex">(initial.config?.harness ?? "claude");
  const [mode, setMode] = useState<AgentMode>(initial.config?.agentMode ?? "manual");
  const [cwd, setCwd] = useState(initial.config?.cwd ?? "");
  const mounted = useRef(true);
  const submitting = useRef(false);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const host = fleet.hosts.find((item) => item.id === hostId);
  const runtime = host && fleet.runtimes[host.id];
  const capabilitiesCurrent = !fleet.error && host?.status === "online" && !runtime?.harnessesError && Boolean(runtime?.harnesses.length);
  const selectedHarness = capabilitiesCurrent && runtime?.harnesses.find((item) => item.id === harness);
  const installed = Boolean(selectedHarness && selectedHarness.installed);
  const available = installed && selectedHarness && selectedHarness.authenticated === true;
  // Mastermind can use the current CLI on an older daemon. The controller
  // verifies the selected provider flags before starting; no mode is downgraded.
  const autoSupported = installed;
  const busy = actions.busy;
  const disabled = busy || actions.uncertain || !available || !cwd || (mode === "auto" && !autoSupported) || actions.state?.revision !== revision;
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (submitting.current || disabled) return;
    submitting.current = true;
    const saved = await actions.configure(revision, { hostId, harness, agentMode: mode, cwd });
    submitting.current = false;
    if (saved && mounted.current) onClose();
  };
  const pause = async () => {
    if (submitting.current) return;
    submitting.current = true;
    const paused = await actions.disable(revision);
    submitting.current = false;
    if (paused && mounted.current) onClose();
  };

  return <Modal title={initial.config ? "Mastermind settings" : "Set up Mastermind"} subtitle="Choose where your coordinator runs. Its conversation stays on that machine." onClose={onClose}>
    <form className="modal-body form-stack mastermind-setup" onSubmit={(event) => void submit(event)}>
      {!fleet.hosts.length ? <div className="mastermind-prerequisite"><Server size={18} /><div><p>Connect a machine, then choose an installed coding agent for Mastermind.</p><button type="button" className="text-button" onClick={onAdd}>Connect a machine <ArrowUpRight size={12} /></button></div></div> : <>
        <label className="field"><span>Machine</span><select aria-label="Machine" value={hostId} disabled={busy} onChange={(event) => { setHostId(event.target.value); setCwd(""); }}>{!host && <option value={hostId}>Machine unavailable</option>}{fleet.hosts.map((item) => <option key={item.id} value={item.id}>{item.name}{item.status !== "online" ? " · Offline" : ""}</option>)}</select></label>
        {host && !capabilitiesCurrent && <div className="mastermind-prerequisite"><CircleAlert size={18} /><div><p>{host.status !== "online" ? "Reconnect this machine before setting up Mastermind." : "Waiting for current agent capabilities. Setup is unavailable while the machine state cannot be verified."}</p><button type="button" className="text-button" onClick={() => onHost(host)}>Open machine <ArrowUpRight size={12} /></button></div></div>}
        <fieldset className="harness-picker" disabled={busy}><legend>Coordinator</legend><div className="harness-options">{(["claude", "codex"] as const).map((id) => {
          const installed = capabilitiesCurrent && runtime?.harnesses.some((item) => item.id === id && item.installed);
          return <label key={id} className={`harness-option ${harness === id ? "selected" : ""} ${!installed ? "unavailable" : ""}`}><input type="radio" name="mastermind-harness" aria-label={provider(id)} checked={harness === id} disabled={!installed} onChange={() => setHarness(id)} /><TerminalIcon size={18} /><strong>{provider(id)}</strong><small>{installed ? "Uses this machine’s sign-in" : "Install on this machine"}</small></label>;
        })}</div></fieldset>
        {host && capabilitiesCurrent && !installed && <p className="mastermind-note">Choose an installed coding agent or <button className="text-button" type="button" onClick={() => onHost(host)}>manage agents on {host.name}</button>.</p>}
        {host && installed && !available && <div className="mastermind-prerequisite"><CircleAlert size={18} /><div><p>{selectedHarness && selectedHarness.authenticated === false ? `${provider(harness)} needs sign-in before Mastermind can start.` : `${provider(harness)} sign-in could not be verified. Check this machine's account before starting Mastermind.`}</p><button className="text-button" type="button" onClick={() => onHost(host)}>Sign in to {provider(harness)} <ArrowUpRight size={12} /></button></div></div>}
        <AgentModePicker value={mode} onChange={setMode} autoSupported={autoSupported} current={capabilitiesCurrent} disabled={busy} />
        {installed && <p className="mastermind-note">Relay verifies that this coding agent supports the selected mode before starting. Existing sessions keep running when this machine uses an older runtime.</p>}
        {host && <FolderPicker key={host.id} host={host} value={cwd} onChange={setCwd} disabled={busy} />}
        <p className="mastermind-note">Saving starts Mastermind using this agent’s existing account. It can coordinate work through Relay and waits for your instructions. Closing Relay keeps the session running.</p>
        {initial.config && <p className="mastermind-note">Changing the machine, agent, folder, or mode starts a new coordinator. The previous session stays in your session list.</p>}
      </>}
      {actions.state?.revision !== revision && !busy && <InlineError error="Mastermind settings changed. Close and reopen this dialog to review the current configuration." />}
      <InlineError error={actions.error} />
      {initial.enabled && initial.config && <p className="mastermind-note">Pause disconnects this panel; the agent keeps running on its machine.</p>}
      <div className="modal-actions">
        {initial.enabled && initial.config && <button type="button" className="button secondary" disabled={busy || actions.uncertain || actions.state?.revision !== revision} onClick={() => void pause()}>Pause Mastermind</button>}
        <button type="button" className="button secondary" onClick={onClose}>Cancel</button>
        <button type="submit" className="button primary" disabled={disabled}>{busy ? <LoaderCircle className="spin" size={15} /> : <BrainCircuit size={15} />}{busy ? "Saving…" : "Save and start Mastermind"}</button>
      </div>
    </form>
  </Modal>;
}
