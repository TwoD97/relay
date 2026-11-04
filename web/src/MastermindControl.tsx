import { useEffect, useState } from "react";
import { BookOpen, Check, MessageCircle, Send, ShieldCheck, Wrench } from "lucide-react";
import { errorMessage, request } from "./api";
import "./MastermindControl.css";
import { DiscordConnection, WhatsAppConnections } from "./RemoteConnections";

type Message = { id: string; source: string; text: string; status: string; reply?: string; delivery?: string; updatedAt: string };
type Lesson = { id: string; problem: string; action: string; outcome: string; evidence: string };
type Connection = { configured: boolean; paired: boolean; bot: string; status: string };
type Control = { revision: number; recovery: boolean; messages: Message[]; lessons: Lesson[]; listenerAt: string; telegram: Connection; discord?: Connection };
type Pair = { bot: string; pairCommand: string; expiresAt: string };
const base = "/api/mastermind/control";
const identity = () => crypto.randomUUID().replaceAll("-", "");

export function MastermindControl() {
  const [state, setState] = useState<Control>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [tab, setTab] = useState<"inbox" | "telegram" | "discord" | "whatsapp" | "lessons">("inbox");
  const [text, setText] = useState("");
  const [token, setToken] = useState("");
  const [pair, setPair] = useState<Pair>();
  const [pending, setPending] = useState<{ id: string; text: string }>();
  useEffect(() => {
    let alive = true;
    const abort = new AbortController();
    const refresh = () => void request<Control>(base, { signal: AbortSignal.any([abort.signal, AbortSignal.timeout(8000)]) }).then(value => { if (alive) setState(value); }).catch(cause => { if (alive && !abort.signal.aborted) setError(errorMessage(cause)); });
    refresh(); const timer = window.setInterval(refresh, 10000);
    return () => { alive = false; abort.abort(); clearInterval(timer); };
  }, []);
  const refresh = async () => setState(await request<Control>(base));
  const change = async (operation: () => Promise<unknown>) => { if (busy) return; setBusy(true); setError(""); try { await operation(); await refresh(); } catch (cause) { setError(errorMessage(cause)); } finally { setBusy(false); } };
  const send = () => change(async () => {
    const instruction = pending ?? { id: identity(), text: text.trim() };
    setPending(instruction);
    await request<Message>(`${base}/messages`, { method: "POST", body: JSON.stringify(instruction) });
    setText(""); setPending(undefined);
  });
  const listening = Boolean(state?.listenerAt && Date.now() - Date.parse(state.listenerAt) < 45000);
  const queued = state?.messages.filter(message => message.status === "queued").length ?? 0;
  return <details className="mastermind-control">
    <summary><MessageCircle size={17}/><strong>Mastermind control</strong><span className={listening ? "control-online" : ""}>{listening ? "Listening" : "Inbox waiting"}{queued ? ` · ${queued} queued` : ""}</span></summary>
    <div className="control-body">
      <nav aria-label="Mastermind control"><button className={tab === "inbox" ? "selected" : ""} onClick={() => setTab("inbox")}><Send size={14}/>Instructions</button><button className={tab === "telegram" ? "selected" : ""} onClick={() => setTab("telegram")}><MessageCircle size={14}/>Telegram</button><button className={tab === "discord" ? "selected" : ""} onClick={() => setTab("discord")}><MessageCircle size={14}/>Discord</button><button className={tab === "whatsapp" ? "selected" : ""} onClick={() => setTab("whatsapp")}><MessageCircle size={14}/>WhatsApp</button><button className={tab === "lessons" ? "selected" : ""} onClick={() => setTab("lessons")}><BookOpen size={14}/>Lessons {state?.lessons.length ?? 0}</button></nav>
      {error && <p className="inline-error" role="alert">{error}</p>}
      {tab === "inbox" && <>
        <p>Send work to your coordinator. Mastermind claims each instruction, updates its tasks and pages, and reports the result here.</p>
        {!listening && <div className="control-help">Start a new Mastermind session to connect its inbox automatically. For an existing coordinator, ask it to read the updated Relay Mastermind skill and run <code>relay control wait</code>. Queued instructions remain saved until it connects.</div>}
        <form onSubmit={event => { event.preventDefault(); void send(); }}><label className="field"><span>Instruction for Mastermind</span><textarea value={text} disabled={busy || Boolean(pending)} onChange={event => setText(event.target.value)} maxLength={8000} rows={3} placeholder="Check the blocked deployment and report what needs my decision…"/></label><div className="control-form-actions">{pending && <small>Delivery may be uncertain. Refresh and inspect the inbox before retrying this same instruction.</small>}<button type="submit" className="button primary" disabled={busy || !state || !text.trim()}><Send size={14}/>{busy ? "Saving…" : pending ? "Check / retry same instruction" : "Send to Mastermind"}</button></div></form>
        <label className="control-recovery"><input type="checkbox" checked={state?.recovery ?? false} disabled={!state || busy} onChange={event => void change(() => request(`${base}/config`, { method: "POST", body: JSON.stringify({ revision: state!.revision, recovery: event.target.checked }) }))}/><span><strong><Wrench size={14}/>Diagnose blocked sessions</strong><small>Mastermind reads the evidence and attempts one bounded recovery. Destructive actions, process interruptions, and restarts require your decision.</small></span></label>
        <div className="control-messages" aria-label="Mastermind inbox">{[...(state?.messages ?? [])].reverse().slice(0,20).map(message => <article key={message.id}><header><span>{({telegram:"Telegram",discord:"Discord","whatsapp-personal":"WhatsApp", "whatsapp-business":"WhatsApp Business",recovery:"Recovery"} as Record<string,string>)[message.source] ?? "You"}</span><span className={`control-message-${message.status}`}>{message.status}</span>{message.status === "queued" && <button className="text-button" disabled={busy} onClick={() => void change(() => request(`${base}/messages/${message.id}/cancel`, { method: "POST" }))}>Cancel</button>}</header><p>{message.text}</p>{(message.delivery === "unknown" || message.delivery === "attempted") && <small>Remote delivery is unknown. This result will not be sent again automatically.</small>}{message.reply && <blockquote><Check size={14}/><span>{message.reply}</span></blockquote>}</article>)}{state?.messages.length === 0 && <p className="control-empty">No instructions yet.</p>}</div>
      </>}
      {tab === "telegram" && <>
        <p>Control Mastermind from your private Telegram chat and answer current provider approval requests. The controller machine must stay running.</p>
        <div className="control-help"><ShieldCheck size={17}/><span>Pair one private account. Instructions, results, and permission-request details travel through your bot. The bot token stays on the controller machine and is excluded from workspace sync.</span></div>
        {state?.telegram.configured && <div className="control-telegram-status"><strong>@{state.telegram.bot} · {state.telegram.paired ? "Paired" : "Waiting for pairing"}</strong><p role="status">{state.telegram.status}</p><button className="button secondary" disabled={busy} onClick={() => void change(async () => { await request(base + "/telegram", { method: "DELETE" }); setPair(undefined); })}>Disconnect Telegram</button></div>}
        {pair && !state?.telegram.paired && <div className="control-pair"><strong>Open a private chat with @{pair.bot}</strong><p>Send this command before {new Date(pair.expiresAt).toLocaleTimeString()}:</p><input aria-label="Telegram pairing command" readOnly value={pair.pairCommand} onFocus={event => event.target.select()}/><small>Pairing only connects this account. Group chats cannot control Relay.</small></div>}
        {!state?.telegram.configured && <form onSubmit={event => { event.preventDefault(); void change(async () => { const result = await request<Pair>(base + "/telegram", { method: "POST", body: JSON.stringify({ token }) }); setToken(""); setPair(result); }); }}><label className="field"><span>BotFather token</span><input type="password" value={token} disabled={busy} onChange={event => setToken(event.target.value)} autoComplete="off" placeholder="Paste your bot token here"/></label><div className="control-form-actions"><button className="button primary" disabled={busy || !token.trim() || !state} type="submit">Connect Telegram</button></div></form>}
        <p className="control-muted"><code>/status</code> shows coordinator and inbox status. <code>/approvals</code> lists requests that can be answered through provider hooks. Plain text queues a new instruction.</p>
      </>}
      {tab === "discord" && <DiscordConnection connection={state?.discord} refresh={refresh}/>}
      {tab === "whatsapp" && <WhatsAppConnections/>}
      {tab === "lessons" && <><p>Verified recovery notes help Mastermind avoid repeating mistakes. These are advisory evidence; they cannot grant permissions or override your instructions.</p><div className="control-lessons">{state?.lessons.map(lesson => <article key={lesson.id}><header><strong>{lesson.problem}</strong><span>{lesson.outcome}</span></header><p>{lesson.action}</p><small>Evidence: {lesson.evidence}</small><button className="text-button" disabled={busy} onClick={() => void change(() => request(`${base}/lessons/${lesson.id}`, { method: "DELETE" }))}>Remove lesson</button></article>)}{state?.lessons.length === 0 && <p className="control-empty">Mastermind records a lesson after a recovery has been checked.</p>}</div></>}
    </div>
  </details>;
}
