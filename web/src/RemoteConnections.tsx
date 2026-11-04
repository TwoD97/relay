import { useEffect, useState } from "react";
import { ShieldCheck } from "lucide-react";
import { errorMessage, request } from "./api";

type Connection = { configured: boolean; paired?: boolean; connected?: boolean; status: string; error?: string; bot?: string };
type Pair = { bot: string; pairCommand: string; expiresAt: string };
const base = "/api/mastermind/control";

export function DiscordConnection({ connection, refresh }: { connection?: Connection; refresh: () => Promise<void> }) {
  const [token, setToken] = useState(""); const [userId, setUserId] = useState("");
  const [pair, setPair] = useState<Pair>(); const [busy, setBusy] = useState(false); const [error, setError] = useState("");
  const change = async (action: () => Promise<void>) => { if (busy) return; setBusy(true); setError(""); try { await action(); await refresh(); } catch (cause) { setError(errorMessage(cause)); } finally { setBusy(false); } };
  return <section className="control-connection" aria-label="Discord connection">
    <p>Use a private conversation with your Discord bot to send instructions, receive results, and answer permission requests.</p>
    <div className="control-help"><ShieldCheck size={17}/><span>Only your paired numeric user ID can control Relay. Server channels, webhooks, and other users are ignored. The bot token stays on the controller machine.</span></div>
    {error && <p className="inline-error" role="alert">{error}</p>}
    {connection?.configured ? <div className="control-telegram-status"><strong>{connection.bot} · {connection.paired ? "Paired" : "Waiting for pairing"}</strong><p role="status">{connection.status}</p><button className="button secondary" disabled={busy} onClick={() => void change(async () => { await request(base+"/discord", { method: "DELETE" }); setPair(undefined); })}>Disconnect Discord</button></div> : <form onSubmit={event => { event.preventDefault(); void change(async () => { const body = JSON.stringify({ token, userId }); setToken(""); setPair(await request<Pair>(base+"/discord", { method: "POST", body })); }); }}>
      <label className="field"><span>Discord bot token</span><input type="password" value={token} onChange={event => setToken(event.target.value)} autoComplete="off" disabled={busy}/></label>
      <label className="field"><span>Your Discord user ID</span><input value={userId} onChange={event => setUserId(event.target.value)} inputMode="numeric" disabled={busy} placeholder="Copy your user ID with Discord Developer Mode"/></label>
      <small className="control-muted">Create a bot in the Discord Developer Portal, add it to a server you use, and allow direct messages. Personal user tokens are not supported.</small>
      <div className="control-form-actions"><button type="submit" className="button primary" disabled={busy || !token.trim() || !userId.trim()}>Connect Discord</button></div>
    </form>}
    {pair && !connection?.paired && <div className="control-pair"><strong>Send this command in your private chat with {pair.bot}</strong><input aria-label="Discord pairing command" value={pair.pairCommand} readOnly onFocus={event => event.target.select()}/><small>Expires at {new Date(pair.expiresAt).toLocaleTimeString()}.</small></div>}
    <p className="control-muted"><code>!status</code> checks the inbox. <code>!approvals</code> lists requests with exact <code>!allow ID</code> and <code>!deny ID</code> replies. Plain text queues instructions. Keep the controller machine running.</p>
  </section>;
}

function useConnection(endpoint: string) {
  const [state, setState] = useState<Connection>(); const [error, setError] = useState(""); const [busy, setBusy] = useState(false);
  useEffect(() => { let alive = true; const abort = new AbortController(); const poll = () => void request<Connection>(endpoint, { signal: AbortSignal.any([abort.signal, AbortSignal.timeout(8000)]) }).then(value => { if (alive) setState(value); }).catch(cause => { if (alive && !abort.signal.aborted) setError(errorMessage(cause)); }); poll(); const timer = window.setInterval(poll,5000); return () => { alive = false; abort.abort(); clearInterval(timer); }; }, [endpoint]);
  const change = async (action: () => Promise<void>) => { if (busy) return; setBusy(true); setError(""); try { await action(); setState(await request<Connection>(endpoint)); } catch (cause) { setError(errorMessage(cause)); } finally { setBusy(false); } };
  return { state, error, busy, change };
}
function PersonalWhatsApp() {
  const endpoint = base+"/whatsapp-personal"; const {state,error,busy,change} = useConnection(endpoint);
  const [phone, setPhone] = useState(""); const [pair,setPair] = useState<{code:string;expiresAt:string}>();
  return <section className="control-connection" aria-label="Personal WhatsApp connection">
    <h3>Personal WhatsApp</h3><p>Link Relay as a device, then use your own “Message yourself” chat. Start each instruction with <code>!relay</code>.</p>
    <div className="control-help"><ShieldCheck size={17}/><span>This uses an independent linked-device client, not Meta’s Business API. Only your own self-chat is accepted. Device credentials stay on this controller and are excluded from peer sync.</span></div>
    {error && <p role="alert" className="inline-error">{error}</p>}{state?.error && <p role="alert" className="inline-error">{state.error}</p>}
    {state?.configured ? <div className="control-telegram-status"><strong>{state.paired ? state.connected ? "Connected" : "Linked · reconnecting" : "Waiting for device link"}</strong><p role="status">{state.status}</p><button className="button secondary" disabled={busy} onClick={() => void change(async () => { await request(endpoint,{method:"DELETE"}); setPair(undefined); })}>Disconnect personal WhatsApp</button></div> : <form onSubmit={event=>{event.preventDefault();void change(async()=>{const body=JSON.stringify({phone});setPhone("");setPair(await request(endpoint,{method:"POST",body}));});}}>
      <label className="field"><span>WhatsApp phone number</span><input type="tel" value={phone} disabled={busy} onChange={event=>setPhone(event.target.value)} autoComplete="off" placeholder="Country code and number"/></label><div className="control-form-actions"><button type="submit" className="button primary" disabled={!state || busy || !phone.trim()}>Link personal WhatsApp</button></div>
    </form>}
    {pair && !state?.paired && <div className="control-pair"><strong>On your phone: Linked devices → Link with phone number</strong><input aria-label="WhatsApp device link code" value={pair.code} readOnly onFocus={event=>event.target.select()}/><small>Use this code before {new Date(pair.expiresAt).toLocaleTimeString()}. It is shown only here.</small></div>}
    <p className="control-muted"><code>!relay status</code>, <code>!relay approvals</code>, <code>!relay allow ID</code>, and <code>!relay deny ID</code>. Other chats and old message history cannot issue commands.</p>
  </section>;
}
function BusinessWhatsApp() {
  const endpoint=base+"/whatsapp-business"; const {state,error,busy,change}=useConnection(endpoint);
  const [form,setForm]=useState({bridgeUrl:"",bridgeToken:"",accessToken:"",phoneNumberId:"",owner:"",graphVersion:""});
  const field=(key:keyof typeof form,label:string,secret=false,placeholder="")=><label className="field" key={key}><span>{label}</span><input type={secret?"password":"text"} value={form[key]} onChange={event=>setForm({...form,[key]:event.target.value})} autoComplete="off" disabled={busy} placeholder={placeholder}/></label>;
  return <section className="control-connection" aria-label="WhatsApp Business connection"><h3>WhatsApp Business Cloud API</h3><p>Connect your Meta Business phone number through a signed webhook bridge on your own HTTPS server.</p>
    <details className="control-help-details"><summary>Bridge setup</summary><p>Run <code>relay whatsapp-bridge --help</code> on your server. Its private configuration contains the app secret, webhook verification token, bridge access token, phone-number ID, and your allowed sender. Put the bridge behind HTTPS and register its webhook in your Meta app. The desktop connects outbound to that bridge.</p><p>Use the Graph API version configured in your Meta app. Send a message from your allowed WhatsApp account to open the reply window; Relay does not send business templates.</p></details>
    {error&&<p role="alert" className="inline-error">{error}</p>}{state?.error&&<p role="alert" className="inline-error">{state.error}</p>}
    {state?.configured?<div className="control-telegram-status"><strong>{state.connected?"Connected":"Configured"}</strong><p role="status">{state.status}</p><button className="button secondary" disabled={busy} onClick={()=>void change(async()=>{await request(endpoint,{method:"DELETE"});})}>Disconnect WhatsApp Business</button></div>:<form onSubmit={event=>{event.preventDefault();void change(async()=>{const body=JSON.stringify(form);setForm({...form,bridgeToken:"",accessToken:""});await request(endpoint,{method:"POST",body});});}}>
      <div className="control-connection-fields">{field("bridgeUrl","Webhook bridge URL",false,"https://your-server.example")}{field("bridgeToken","Bridge access token",true)}{field("accessToken","Meta Cloud access token",true)}{field("phoneNumberId","Meta phone-number ID")}{field("owner","Allowed sender number",false,"Country code and digits only")}{field("graphVersion","Meta Graph API version",false,"vNN.N from your Meta app")}</div>
      <div className="control-form-actions"><button className="button primary" type="submit" disabled={!state||busy||Object.values(form).some(value=>!value.trim())}>Connect WhatsApp Business</button></div>
    </form>}
    <p className="control-muted">Send instructions as plain text. <code>!status</code>, <code>!approvals</code>, <code>!allow ID</code>, and <code>!deny ID</code> manage current requests. Replies use the active customer-service window.</p>
  </section>;
}
export function WhatsAppConnections() {
  const [kind,setKind]=useState<"personal"|"business">("personal");
  return <><div className="control-connection-choice" role="group" aria-label="WhatsApp account type"><button aria-pressed={kind==="personal"} onClick={()=>setKind("personal")}>Personal account</button><button aria-pressed={kind==="business"} onClick={()=>setKind("business")}>Business Cloud API</button></div>{kind==="personal"?<PersonalWhatsApp/>:<BusinessWhatsApp/>}</>;
}
