import { useEffect, useRef, useState } from "react";
import { ArrowRightLeft, Check, LoaderCircle, Server } from "lucide-react";
import { ApiError, errorMessage, request } from "./api";
import { Modal } from "./Modal";
import type { FleetState } from "./types";
import "./MastermindMigration.css";

type Operation = { id: string; hostId: string; status: "preparing" | "quiescing" | "starting" | "active" | "uncertain" | "failed"; stage: string; error?: string; updatedAt: string };
type Migration = { operation?: Operation; hosted?: { hostId: string; coordinatorId: string }; detail?: string };
type Plan = { hostId: string; fingerprint: string; sourceRevision: number; harness: string; agentMode: string; counts: { pages: number; tasks: number; lessons: number; queuedMessages: number }; blockers: string[]; warnings: string[] };
type Pending = { id: string; hostId: string };
const endpoint = "/api/mastermind/migration";
// A local UI intent only. The controller journal remains the authority; this
// prevents a lost browser response followed by navigation from offering a replay.
const intentKey = "relay.mastermind.migration.pending.v1";
function pendingIntent(): Pending | undefined {
  try { const value = JSON.parse(sessionStorage.getItem(intentKey) ?? "null"); if (/^[a-f0-9]{32}$/.test(value?.id) && typeof value.hostId === "string" && value.hostId.length < 129) return value; } catch { /* Browser state is optional. */ }
}
export function MastermindMigration({ fleet, onChanged }: { fleet: FleetState; onChanged: () => void }) {
  const [state, setState] = useState<Migration>(); const [error, setError] = useState("");
  const [open, setOpen] = useState(false); const [hostId, setHostId] = useState(""); const [plan, setPlan] = useState<Plan>();
  const [busy, setBusy] = useState(false); const [confirmed, setConfirmed] = useState(false); const [unknown, setUnknown] = useState<Pending | undefined>(pendingIntent);
  const mounted = useRef(false); const generation = useRef(0); const inFlight = useRef(false);
  const unknownRef = useRef(unknown); unknownRef.current = unknown;
  const acceptedGeneration = useRef(0);
  const refreshed = useRef(""); const onChangeRef = useRef(onChanged); onChangeRef.current = onChanged;
  const selectedIdentity = useRef("");
  const selectedHost = fleet.hosts.find(host => host.id === hostId);
  const identity = selectedHost ? `${selectedHost.id}/${selectedHost.createdAt}/${selectedHost.target}/${selectedHost.port}` : "";
  selectedIdentity.current = identity;
  const remember = (value: Pending | undefined) => { unknownRef.current = value; setUnknown(value); try { if (value) sessionStorage.setItem(intentKey,JSON.stringify(value)); else sessionStorage.removeItem(intentKey); } catch { /* Controller still owns durable intent. */ } };
  useEffect(() => {
    mounted.current = true; let live = true, polling = false; const abort = new AbortController();
    const poll = async () => { if (polling) return; polling=true; const requestGeneration=acceptedGeneration.current; try {
      const next = await request<Migration>(endpoint, { signal: AbortSignal.any([abort.signal,AbortSignal.timeout(10000)]) });
      if (!live || requestGeneration!==acceptedGeneration.current) return; setState(next);
      const pending = unknownRef.current;
      if (pending && next.operation?.id === pending.id && next.operation.hostId === pending.hostId) { remember(undefined); setOpen(false); setPlan(undefined); }
      if (next.operation?.status === "active" && refreshed.current !== next.operation.id) { refreshed.current=next.operation.id; onChangeRef.current(); }
    } catch(cause) { if(live&&!abort.signal.aborted) setError(errorMessage(cause)); } finally { polling=false; } };
    void poll(); const timer=window.setInterval(()=>void poll(),3000);
    return()=>{live=false;mounted.current=false;generation.current++;abort.abort();clearInterval(timer);};
  },[]);
  useEffect(()=>{generation.current++;setPlan(undefined);setConfirmed(false);},[identity]);
  const targets = fleet.hosts.filter(host=>host.id!=="local" && host.id!==state?.hosted?.hostId);
  const locked = busy || Boolean(unknown) || Boolean(state?.operation && state.operation.status !== "failed" && state.operation.status !== "active");
  const review = async()=>{
    if(inFlight.current || !selectedHost || selectedHost.status!=="online" || locked)return;
    inFlight.current=true;setBusy(true);setError("");setPlan(undefined);setConfirmed(false);
    const requestGeneration=++generation.current, requestIdentity=identity;
    try { const value=await request<Plan>(endpoint+"/preflight",{method:"POST",body:JSON.stringify({hostId}),signal:AbortSignal.timeout(100000)});
      if(!mounted.current||generation.current!==requestGeneration||selectedIdentity.current!==requestIdentity)return;
      if(value.hostId!==hostId||!value.fingerprint||!Number.isSafeInteger(value.sourceRevision)||!Array.isArray(value.blockers)||!Array.isArray(value.warnings)||!value.counts)throw Error("The destination plan could not be verified.");
      setPlan(value);
    }catch(cause){if(mounted.current&&generation.current===requestGeneration)setError(errorMessage(cause));}
    finally{inFlight.current=false;if(mounted.current)setBusy(false);}
  };
  const start=async()=>{
    if(inFlight.current||!plan||!confirmed||plan.blockers.length||locked||selectedHost?.status!=="online")return;
    const intent={id:crypto.randomUUID().replaceAll("-",""),hostId:plan.hostId};
    inFlight.current=true;setBusy(true);setError("");remember(intent);
    try{const operation=await request<Operation>(endpoint+"/start",{method:"POST",body:JSON.stringify({...intent,fingerprint:plan.fingerprint})});
      if(!mounted.current)return;
      if(operation.id!==intent.id||operation.hostId!==intent.hostId)throw Error("Migration acceptance could not be verified. Inspect its status before another attempt.");
      acceptedGeneration.current++;remember(undefined);setState({operation});setOpen(false);setPlan(undefined);onChangeRef.current();
    }catch(cause){if(mounted.current){if(cause instanceof ApiError&&cause.status>=400&&cause.status<500)remember(undefined);setError(errorMessage(cause));}}
    finally{inFlight.current=false;if(mounted.current)setBusy(false);}
  };
  const destination=fleet.hosts.find(host=>host.id===state?.hosted?.hostId);
  return <section className="mastermind-migration" aria-label="Mastermind location">
    <div className="migration-summary"><Server size={18}/><div><strong>{state?.hosted ? `Hosted on ${destination?.name ?? "another machine"}` : "Keep Mastermind running elsewhere"}</strong><p>{state?.hosted ? "Its controller, inbox, and workspace run there while this desktop is closed." : "Move your coordinator and workspace to an always-on machine."}</p></div><button className="button secondary compact" disabled={!state||locked||Boolean(state.hosted)||!fleet.mastermind?.config||targets.length===0} title={state?.hosted ? "Moving an already hosted coordinator again is not available yet" : undefined} onClick={()=>{setError("");setHostId(targets.find(host=>host.status==="online")?.id??targets[0]?.id??"");setOpen(true);}}><ArrowRightLeft size={14}/>Move Mastermind</button></div>
    {state?.operation&&<p role="status" className="migration-status">{state.operation.status==="active"?<Check size={15}/>:!["failed","uncertain"].includes(state.operation.status)?<LoaderCircle className="spin" size={15}/>:null}{state.operation.stage}{state.operation.error&&<span>{state.operation.error}</span>}</p>}
    {state?.hosted&&<p className="control-muted">This desktop reconnects to the hosted controller. Moving it again or adopting it from a different desktop is not available yet.</p>}{state?.detail&&<p className="control-muted">{state.detail}</p>}{error&&!open&&<p className="inline-error" role="alert">{error}</p>}
    {unknown&&<p role="status">The move request is unconfirmed. Relay will only check its saved status; it will not repeat the move.</p>}
    {open&&<Modal title="Move Mastermind" subtitle="Prepare a persistent controller on another machine." onClose={()=>{if(!busy){generation.current++;setOpen(false);setPlan(undefined);}}}>
      <div className="modal-body migration-body"><label className="field"><span>Destination machine</span><select value={hostId} disabled={busy||Boolean(unknown)} onChange={event=>{setHostId(event.target.value);setPlan(undefined);setError("");}}>{targets.map(host=><option key={host.id} value={host.id} disabled={host.status!=="online"}>{host.name}{host.status!=="online"?" · offline":""}</option>)}</select></label>
        <p>Relay checks this machine’s agent sign-in and access to your other hosts. Preparation does not start another coordinator.</p>
        {!plan&&<button className="button secondary" disabled={locked||selectedHost?.status!=="online"} onClick={()=>void review()}>{busy?<LoaderCircle className="spin" size={14}/>:<Server size={14}/>}Check destination</button>}
        {plan&&<><dl className="migration-counts"><div><dt>Pages</dt><dd>{plan.counts.pages}</dd></div><div><dt>Tasks</dt><dd>{plan.counts.tasks}</dd></div><div><dt>Lessons</dt><dd>{plan.counts.lessons}</dd></div><div><dt>Queued instructions</dt><dd>{plan.counts.queuedMessages}</dd></div></dl>
          <p>The source coordinator stops. {plan.harness==="claude"?"Claude Code":"Codex"} starts a fresh conversation on the destination using your saved handoff, tasks, pages, and lessons. Source terminal output remains available. Project files and full provider history stay on their original machines.</p>
          <p>Provider sign-in, SSH keys, and messaging credentials stay on their current machine. Configure remote-control channels on the destination after the move.</p>
          {plan.warnings.map(warning=><p className="migration-warning" key={warning}>{warning}</p>)}{plan.blockers.map(blocker=><p className="inline-error" role="alert" key={blocker}>{blocker}</p>)}
          <label className="migration-confirm"><input type="checkbox" checked={confirmed} disabled={locked||Boolean(plan.blockers.length)} onChange={event=>setConfirmed(event.target.checked)}/><span>I’m ready to stop the source coordinator and continue from the saved handoff on this machine.</span></label>
        </>}{error&&<p className="inline-error" role="alert">{error}</p>}{unknown&&<p role="status">The request may have completed. Keep the source running until Relay confirms the destination.</p>}
      </div><div className="modal-footer"><button className="button secondary" disabled={busy} onClick={()=>{generation.current++;setOpen(false);}}>Cancel</button><button className="button primary" disabled={!plan||!confirmed||Boolean(plan.blockers.length)||locked} onClick={()=>void start()}>Move and start Mastermind</button></div>
    </Modal>}
  </section>;
}
