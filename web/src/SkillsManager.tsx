import { useEffect, useRef, useState } from "react";
import { BookOpen, Check, Download, FileUp, LoaderCircle, RefreshCw, ShieldCheck, Trash2, Upload } from "lucide-react";
import { errorMessage, request } from "./api";
import { Confirm, InlineError } from "./Forms";
import { Modal } from "./Modal";
import type { FleetState } from "./types";
import "./SkillsManager.css";

type Skill = { name: string; description: string; digest: string; bytes: number; files: number };
type Catalog = { revision: number; entries: Skill[] };
type Inspection = Skill & { paths: string[] };
type Provider = { provider: "claude" | "codex"; status: "installed" | "absent" | "conflict" | "uncertain"; digest?: string; detail?: string };
type Deployment = { name: string; providers: Provider[] };
type DeploymentResult = { protocol: 1; deployments: Deployment[] };
const size = (bytes: number) => bytes < 1024 ? `${bytes} B` : `${(bytes / 1024).toFixed(1)} KiB`;
const upload = (file: File) => ({ body: file, headers: { "Content-Type": file.name.toLowerCase().endsWith(".zip") ? "application/zip" : "text/markdown" }, method: "POST" });

export function SkillsManager({ fleet }: { fleet: FleetState }) {
  const [catalog, setCatalog] = useState<Catalog>();
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [uncertain, setUncertain] = useState(false);
  const [hostId, setHostId] = useState(() => fleet.hosts.find((host) => host.id === "local")?.id ?? fleet.hosts[0]?.id ?? "");
  const [selected, setSelected] = useState<string>();
  const [deployment, setDeployment] = useState<{ hostId: string; name: string; result: DeploymentResult }>();
  const [deploymentError, setDeploymentError] = useState<string>();
  const [inspection, setInspection] = useState<{ file: File; info: Inspection; revision: number }>();
  const [removal, setRemoval] = useState<{ skill: Skill; revision: number; hostId?: string }>();
  const input = useRef<HTMLInputElement>(null);
  const current = useRef({ hostId, selected, mounted: true });
  current.current.hostId = hostId; current.current.selected = selected;
  useEffect(() => { current.current.mounted = true; return () => { current.current.mounted = false; }; }, []);
  const reload = async () => {
    try { const value = await request<Catalog>("/api/skills"); if (current.current.mounted) { setCatalog(value); setError(undefined); setUncertain(false); } }
    catch (cause) { if (current.current.mounted) setError(errorMessage(cause)); }
  };
  useEffect(() => { void reload(); }, []);
  const skill = catalog?.entries.find((entry) => entry.name === selected) ?? (!selected ? catalog?.entries[0] : undefined);
  const host = fleet.hosts.find((item) => item.id === hostId);
  const locked = busy || uncertain || !catalog || Boolean(error || fleet.error);
  const status = deployment?.hostId === hostId && deployment.name === skill?.name ? deployment.result.deployments.find((item) => item.name === skill?.name) : undefined;
  const installedDigests = [...new Set(status?.providers.filter((item) => item.status === "installed").map((item) => item.digest).filter(Boolean) ?? [])];
  const checking = useRef(0);
  useEffect(() => {
    const generation = ++checking.current;
    setDeployment(undefined); setDeploymentError(undefined);
    if (!skill || !host || host.status !== "online") return;
    const controller = new AbortController();
    void request<DeploymentResult>(`/api/hosts/${encodeURIComponent(hostId)}/skills?name=${encodeURIComponent(skill.name)}`, { signal: AbortSignal.any([controller.signal, AbortSignal.timeout(10000)]) }).then((result) => {
      if (generation === checking.current) setDeployment({ hostId, name: skill.name, result });
    }).catch((cause) => { if (!controller.signal.aborted && generation === checking.current) setDeploymentError(errorMessage(cause)); });
    return () => controller.abort();
  }, [hostId, host?.status, skill?.name]);
  const hostOperation = async (action: "check" | "deploy" | "reconcile" | "remove", chosen = skill, target = hostId) => {
    if (!chosen || busy) return;
    const generation = ++checking.current;
    setBusy(true); setDeploymentError(undefined);
    const path = `/api/hosts/${encodeURIComponent(target)}/skills` + (action === "check" ? `/check?name=${encodeURIComponent(chosen.name)}` : `/${encodeURIComponent(chosen.name)}${action === "remove" ? "" : `/${action}`}`);
    try {
      const result = await request<DeploymentResult>(path, { method: action === "remove" ? "DELETE" : "POST", ...(action === "check" ? {} : { body: JSON.stringify({ digest: chosen.digest }) }), signal: AbortSignal.timeout(190000) });
      if (current.current.mounted && generation === checking.current && current.current.hostId === target) setDeployment({ hostId: target, name: chosen.name, result });
    } catch (cause) {
      if (current.current.mounted && generation === checking.current && current.current.hostId === target) { setDeployment(undefined); setDeploymentError(errorMessage(cause)); }
      throw cause;
    } finally { if (current.current.mounted) setBusy(false); }
  };
  const inspect = async (file?: File) => {
    if (!file || locked) return;
    if (file.size > 4 * 1024 * 1024) { setError("Skill imports must be no larger than 4 MiB."); return; }
    setBusy(true); setError(undefined);
    try { const info = await request<Inspection>("/api/skills/inspect", upload(file)); if (current.current.mounted) setInspection({ file, info, revision: catalog!.revision }); }
    catch (cause) { if (current.current.mounted) setError(errorMessage(cause)); }
    finally { if (current.current.mounted) setBusy(false); if (input.current) input.current.value = ""; }
  };
  const importSkill = async () => {
    if (!inspection || busy || !catalog) return;
    const { file, info, revision } = inspection;
    const previous = catalog.entries.find((entry) => entry.name === info.name);
    setBusy(true); setError(undefined);
    try {
      const query = new URLSearchParams({ revision: String(revision), ...(previous ? { expectedDigest: previous.digest } : {}) });
      const value = await request<Catalog>(`/api/skills/import?${query}`, upload(file));
      if (current.current.mounted) { setCatalog(value); setSelected(info.name); setInspection(undefined); }
    } catch (cause) { if (current.current.mounted) { setError(errorMessage(cause)); setUncertain(true); setInspection(undefined); } }
    finally { if (current.current.mounted) setBusy(false); }
  };
  return <section className="skills-manager" aria-label="Shared skills">
    <header className="skills-heading"><div><span className="workspace-eyebrow"><BookOpen size={14} /> SHARED INSTRUCTIONS</span><h1>Skills</h1><p>One reviewed package, available to Claude Code and Codex on the machines you choose.</p></div><div><button className="icon-button" aria-label="Reload skill library" disabled={busy} onClick={() => void reload()}><RefreshCw size={17} /></button><button className="button primary compact" disabled={locked} onClick={() => input.current?.click()}><FileUp size={15} />Import skill</button><input ref={input} className="sr-only" type="file" accept=".zip,.md,text/markdown,application/zip" aria-label="Skill package" onChange={(event) => void inspect(event.target.files?.[0])} /></div></header>
    <InlineError error={error} />
    {uncertain && <p role="status" className="skills-warning">The last library change is unconfirmed and was not repeated. Reload the library and review its current contents before another change.</p>}
    <div className="skills-layout">
      <nav className="skills-catalog" aria-label="Skill library">{!catalog ? <p>Loading skills…</p> : !catalog.entries.length ? <div className="skills-empty"><BookOpen size={26} /><h2>Your shared skill library</h2><p>Import SKILL.md or a ZIP containing one skill and its supporting files. Up to 64 files and 4 MiB per skill.</p></div> : catalog.entries.map((entry) => <button key={entry.name} aria-current={skill?.name === entry.name ? "page" : undefined} onClick={() => setSelected(entry.name)} disabled={busy}><strong>{entry.name}</strong><small>{entry.files} files · {size(entry.bytes)}</small></button>)}</nav>
      {skill && <article className="skill-detail" aria-label={`Skill ${skill.name}`}>
        <header><div><h2>{skill.name}</h2><p>{skill.description}</p></div><a className="icon-button" aria-label={`Export ${skill.name}`} title="Export skill ZIP" href={`/api/skills/${encodeURIComponent(skill.name)}/export?digest=${skill.digest}`} download={`${skill.name}.zip`}><Download size={17} /></a><button className="icon-button" aria-label={`Remove ${skill.name} from library`} title="Remove from library" disabled={locked} onClick={() => setRemoval({ skill, revision: catalog!.revision })}><Trash2 size={16} /></button></header>
        <dl className="skill-facts"><div><dt>Package</dt><dd>{skill.files} files · {size(skill.bytes)}</dd></div><div><dt>SHA-256</dt><dd title={skill.digest}>{skill.digest}</dd></div></dl>
        <section className="skill-deployment" aria-label="Skill deployment"><div className="skill-machine"><label className="field"><span>Machine</span><select aria-label="Skill machine" value={hostId} disabled={busy} onChange={(event) => setHostId(event.target.value)}><option value="">Choose a machine</option>{fleet.hosts.map((item) => <option key={item.id} value={item.id}>{item.name}{item.status !== "online" ? " · Offline" : ""}</option>)}</select></label><button className="button secondary compact" disabled={busy || !host || host.status !== "online"} onClick={() => void hostOperation("check").catch(() => {})}><RefreshCw size={14} />Check deployment</button></div>
          <p className="field-hint">Deploys the same package to this user’s personal Claude Code and Codex skill directories. Existing or modified skills are preserved. No imported files run during installation.</p>
          <InlineError error={deploymentError} />
          <div className="skill-providers">{(["claude", "codex"] as const).map((provider) => { const fact = status?.providers.find((item) => item.provider === provider); return <div key={provider}><ShieldCheck size={17} /><div><strong>{provider === "claude" ? "Claude Code" : "Codex"}</strong><span>{!host || host.status !== "online" ? "Machine unavailable" : !fact ? "Not checked" : fact.status === "installed" ? fact.digest === skill.digest ? "Current package installed" : "Different managed version installed" : fact.status === "absent" ? "Not installed" : fact.status === "conflict" ? "Existing files preserved" : "Unconfirmed operation"}</span>{fact?.detail && <small>{fact.detail}</small>}</div>{fact?.status === "installed" && fact.digest === skill.digest && <Check size={15} />}</div>; })}</div>
          <div className="skill-deploy-actions"><button className="button primary compact" disabled={locked || !host || host.status !== "online" || status?.providers.some((item) => item.status === "conflict" || item.status === "uncertain")} onClick={() => void hostOperation("deploy").catch(() => {})}>{busy ? <LoaderCircle className="spin" size={15} /> : <Upload size={15} />}Deploy to both</button>{status?.providers.some((item) => item.status === "uncertain") && <button className="button secondary compact" disabled={busy} onClick={() => void hostOperation("reconcile").catch(() => {})}>Review deployment</button>}{status?.providers.some((item) => item.status === "installed") && <button className="button ghost-danger compact" disabled={locked || installedDigests.length !== 1} onClick={() => setRemoval({ skill: { ...skill, digest: installedDigests[0]! }, revision: catalog!.revision, hostId })}>Remove from machine</button>}</div>
          <p className="field-hint">Provider-specific features and project overrides may differ. Existing agent sessions keep running; newly deployed skills may require a provider refresh or a new session.</p>
        </section>
      </article>}
    </div>
    {inspection && <Modal title="Import shared skill" subtitle="Review the package before adding it to your library. Importing does not deploy or execute it." onClose={() => { if (!busy) setInspection(undefined); }}><div className="modal-body form-stack"><h3>{inspection.info.name}</h3><p>{inspection.info.description}</p><p>{inspection.info.files} files · {size(inspection.info.bytes)}</p><ul className="skill-file-list">{inspection.info.paths.map((path) => <li key={path}><code>{path}</code></li>)}</ul>{catalog?.entries.some((entry) => entry.name === inspection.info.name) && <p className="skills-warning">This replaces the library version of this skill. Installed copies on machines are unchanged until you explicitly deploy.</p>}<div className="modal-actions"><button className="button secondary" disabled={busy} onClick={() => setInspection(undefined)}>Cancel</button><button className="button primary" disabled={busy || inspection.revision !== catalog?.revision} onClick={() => void importSkill()}>{busy ? "Importing…" : catalog?.entries.some((entry) => entry.name === inspection.info.name) ? "Replace library version" : "Import into library"}</button></div></div></Modal>}
    {removal && <Confirm title={removal.hostId ? "Remove installed skill?" : "Remove skill from library?"} description={removal.hostId ? "Only exact Relay-managed copies of this package will be removed from Claude Code and Codex on this machine. Edited or unrelated files are preserved. Running sessions are not restarted." : "This removes the library entry. Skills already deployed on machines and running sessions are kept."} action="Remove skill" onClose={() => setRemoval(undefined)} onConfirm={async () => {
      if (removal.hostId) { await hostOperation("remove", removal.skill, removal.hostId); return; }
      setBusy(true);
      try { const value = await request<Catalog>(`/api/skills/${encodeURIComponent(removal.skill.name)}`, { method: "DELETE", body: JSON.stringify({ revision: removal.revision, digest: removal.skill.digest }) }); if (current.current.mounted) { setCatalog(value); setSelected(undefined); } }
      catch (cause) { if (current.current.mounted) { setUncertain(true); setError(errorMessage(cause)); } throw cause; }
      finally { if (current.current.mounted) setBusy(false); }
    }} />}
  </section>;
}
