import { useEffect, useRef, useState } from "react";
import { X } from "lucide-react";
import { request } from "./api";
import type { FleetState, Host, Session } from "./types";
import "./AgentWindowRequests.css";

type ViewRequest = { id: string; sessionId: string; sessionCreatedAt: string; createdAt: string; expiresAt: string; status: string };
type NativeDesktop = { __RELAY_PRIMARY_WINDOW__?: boolean; __TAURI_INTERNALS__?: { invoke: (command: string, args: unknown) => Promise<unknown> } };
const token = () => Array.from(crypto.getRandomValues(new Uint8Array(16)), (value) => value.toString(16).padStart(2, "0")).join("");
const identity = (host: Host) => JSON.stringify([host.id, host.createdAt, host.target, host.port]);
const validID = (value: unknown): value is string => typeof value === "string" && /^[a-f0-9]{32}$/.test(value);

// Only the native primary process consumes requests. An expanded route is still
// primary; a detached viewer stays ineligible even if its hash changes.
export function AgentWindowRequests({ fleet }: { fleet: FleetState }) {
  const latest = useRef(fleet); latest.current = fleet;
  const [notice, setNotice] = useState<string>();
  useEffect(() => {
    const desktop = window as unknown as NativeDesktop;
    const native = desktop.__TAURI_INTERNALS__;
    if (desktop.__RELAY_PRIMARY_WINDOW__ !== true || !native) return;
    const controller = new AbortController();
    const clientId = token();
    const checking = new Set<string>();
    const attempted = new Set<string>();
    let opening = false, cursor = 0;
    let openedAt: number[] = [];
    const activeHost = (saved: Host) => {
      const state = latest.current;
      const host = state.hosts.find((item) => item.id === saved.id);
      const runtime = state.runtimes[saved.id];
      return !controller.signal.aborted && document.visibilityState === "visible" && !state.error && host?.status === "online" && identity(host) === identity(saved) && runtime && !runtime.sessionsError && runtime.fetchedAt !== undefined && Date.now() - runtime.fetchedAt < 12000 ? runtime : undefined;
    };
    const sessionFor = (host: Host, item: ViewRequest) => activeHost(host)?.sessions.find((session) => session.id === item.sessionId && session.createdAt === item.sessionCreatedAt && !session.purpose);
    const api = <T,>(path: string, method = "GET", body?: unknown) => request<T>(path, { method, ...(body === undefined ? {} : { body: JSON.stringify(body) }), signal: AbortSignal.any([controller.signal, AbortSignal.timeout(8000)]) });
    const consume = async (host: Host, item: ViewRequest) => {
      const key = `${identity(host)}:${item.id}`;
      openedAt = openedAt.filter((time) => Date.now() - time < 30000);
      if (opening || openedAt.length >= 2 || attempted.has(key) || !sessionFor(host, item)) return;
      opening = true;
      attempted.add(key);
      // Bounded lifetime cache. The runtime's claim remains authoritative after
      // this process forgets an old ID or closes; a claim is never replayed.
      if (attempted.size > 256) attempted.delete(attempted.values().next().value!);
      const path = `/api/hosts/${encodeURIComponent(host.id)}/runtime/views/${item.id}`;
      try {
        let claimed: ViewRequest;
        try { claimed = await api<ViewRequest>(`${path}/claim`, "POST", { clientId }); }
        catch { return; }
        if (claimed.id !== item.id || claimed.sessionId !== item.sessionId || claimed.sessionCreatedAt !== item.sessionCreatedAt || claimed.status !== "claimed") {
          if (!controller.signal.aborted) setNotice("An agent window request could not be verified. No window was opened and the request will not be repeated.");
          return;
        }
        if (!sessionFor(host, item)) { await api(`${path}/complete`, "POST", { clientId, status: "failed" }).catch(() => {}); return; }
        // Re-read identity after claiming. This never claims terminal input or
        // uses a provider-supplied URL, title, command or environment variable.
        let sessions: Session[];
        try { sessions = await api<Session[]>(`/api/hosts/${encodeURIComponent(host.id)}/runtime/sessions`); }
        catch { return; }
        if (!sessionFor(host, item) || !Array.isArray(sessions) || !sessions.some((session) => session.id === item.sessionId && session.createdAt === item.sessionCreatedAt && !session.purpose)) {
          await api(`${path}/complete`, "POST", { clientId, status: "failed" }).catch(() => {}); return;
        }
        openedAt.push(Date.now());
        let timeout: ReturnType<typeof setTimeout> | undefined;
        try {
          await Promise.race([native.invoke("open_session_window", { hostId: host.id, sessionId: item.sessionId }), new Promise<never>((_, reject) => { timeout = setTimeout(() => reject(new Error("Native window response timed out")), 15000); })]);
        } catch {
          // A rejected/missing native acknowledgment does not prove that no
          // window spawned. Leave claimed -> uncertain; never use a fallback.
          if (!controller.signal.aborted) setNotice("An agent requested a terminal window, but opening it was not confirmed. Relay will not repeat the request. Open the session manually if needed.");
          return;
        } finally { if (timeout) clearTimeout(timeout); }
        await api(`${path}/complete`, "POST", { clientId, status: "opened" }).catch(() => {
          if (!controller.signal.aborted) setNotice("The terminal window opened, but its request acknowledgment was not confirmed. Relay will not open it again automatically.");
        });
      } finally { opening = false; }
    };
    const pollHost = async (host: Host) => {
      const key = identity(host);
      if (checking.has(key) || checking.size >= 4 || !activeHost(host)) return;
      checking.add(key);
      try {
        // Fleet health is optional on compatible remote runtimes. Probe this
        // operation's capability directly, scoped to the current host identity.
        const health = await api<{ capabilities?: { viewRequestsV1?: boolean } }>(`/api/hosts/${encodeURIComponent(host.id)}/runtime/health`);
        if (!activeHost(host) || health.capabilities?.viewRequestsV1 !== true) return;
        const value = await api<{ requests: ViewRequest[] }>(`/api/hosts/${encodeURIComponent(host.id)}/runtime/views`);
        if (!activeHost(host) || !Array.isArray(value.requests) || value.requests.length > 64) return;
        for (const item of value.requests) {
          if (item.status !== "pending" || !validID(item.id) || !validID(item.sessionId) || typeof item.sessionCreatedAt !== "string" || !Number.isFinite(Date.parse(item.expiresAt)) || Date.parse(item.expiresAt) <= Date.now()) continue;
          await consume(host, item);
        }
      } catch { /* A host can disappear or retain a compatible older runtime. */ }
      finally { checking.delete(key); }
    };
    const tick = () => {
      const hosts = latest.current.hosts;
      for (let index = 0; index < hosts.length; index++) { const host = hosts[(cursor + index) % hosts.length]; void pollHost(host); }
      cursor = hosts.length ? (cursor + 4) % hosts.length : 0;
    };
    const timer = setInterval(tick, 3000);
    document.addEventListener("visibilitychange", tick);
    tick();
    return () => { controller.abort(); clearInterval(timer); document.removeEventListener("visibilitychange", tick); };
  }, []);
  return notice ? <aside className="agent-window-notice" role="status"><span>{notice}</span><button className="icon-button" aria-label="Dismiss window request notice" onClick={() => setNotice(undefined)}><X size={15} /></button></aside> : null;
}
