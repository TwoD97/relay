import type { DirectoryListing, Harness, HarnessAction, Host, MaintenanceJob, ProjectContextReport, Session } from "./types";

export class ApiError extends Error {
  constructor(message: string, readonly status: number) { super(message); }
}

let csrf = "";
let bootstrapPending: Promise<{ csrf: string; version: string }> | null = null;

export async function request<T>(path: string, options?: RequestInit): Promise<T> {
  const headers = new Headers(options?.headers);
  headers.set("Accept", "application/json");
  if (options?.body) headers.set("Content-Type", "application/json");
  if (options?.method && options.method !== "GET") {
    if (!csrf) await bootstrap();
    headers.set("X-Relay-CSRF", csrf);
  }
  const mutation = Boolean(options?.method && options.method !== "GET");
  let response: Response;
  try {
    response = await fetch(path, { ...options, headers, credentials: "same-origin", signal: options?.signal ?? AbortSignal.timeout(mutation ? 30000 : 8000) });
  } catch (error) {
    if (mutation) throw new ApiError("The connection ended before Relay answered. The action may have completed; check the machine before trying again.", 0);
    throw error;
  }
  if (!response.ok) {
    // A different tab may reauthenticate after a controller restart. Refresh
    // bootstrap facts on the next request, but never replay this mutation.
    if (response.status === 401 || response.status === 403) { csrf = ""; bootstrapPending = null; }
    const raw = await response.text();
    if (response.headers.get("Content-Type")?.includes("text/html")) throw new ApiError("Relay's API returned a web page. Open Relay from the desktop app's Open in browser menu, or start its controller with relay ui." + (mutation ? " The action may have completed; check the machine before trying again." : ""), response.status);
    let message = raw || `Request failed (${response.status}).`;
    try {
      const parsed = JSON.parse(raw) as { error?: string | { message?: string }; message?: string };
      message = typeof parsed.error === "string" ? parsed.error : parsed.error?.message || parsed.message || message;
    } catch { /* A plain-text diagnostic is valid too. */ }
    throw new ApiError(response.status === 401 ? "This browser is not connected to your Relay controller." : message, response.status);
  }
  if (response.status === 204) return undefined as T;
  const text = await response.text();
  if (response.headers.get("Content-Type")?.includes("text/html") || /^\s*</.test(text)) {
    throw new ApiError("Relay's API returned a web page. Open Relay from the desktop app's Open in browser menu, or start its controller with relay ui." + (mutation ? " The action may have completed; check the machine before trying again." : ""), 502);
  }
  try { return text ? JSON.parse(text) as T : undefined as T; }
  catch { throw new ApiError("Relay returned an invalid API response. Check the controller connection." + (mutation ? " The action may have completed; check the machine before trying again." : ""), 502); }
}

export function bootstrap() {
  if (!bootstrapPending) {
    bootstrapPending = request<{ csrf: string; version: string }>("/api/bootstrap")
      .then((data) => { csrf = data.csrf; return data; })
      .catch((error) => { bootstrapPending = null; throw error; });
  }
  return bootstrapPending;
}

export const runtimePath = (host: string, suffix: string) => `/api/hosts/${encodeURIComponent(host)}/runtime/${suffix}`;
export const hostPath = (host: string, suffix = "") => `/api/hosts/${encodeURIComponent(host)}${suffix ? `/${suffix}` : ""}`;
export const api = {
  state: () => request<{ version: string; hosts: Host[]; maintenanceJobs?: MaintenanceJob[] }>("/api/state"),
  maintainHarness: (host: string, harness: string, action: "update" | "repair") => request<MaintenanceJob>(hostPath(host, `harnesses/${encodeURIComponent(harness)}/${action}`), { method: "POST" }),
  prepareProjectContext: (host: string, path: string) => request<ProjectContextReport>(hostPath(host, "project-context"), { method: "POST", body: JSON.stringify({ path }), signal: AbortSignal.timeout(190000) }),
  reconnect: () => request<{ queued: number }>("/api/reconnect", { method: "POST" }),
  repairRuntime: (host: string) => request<Host>(hostPath(host, "repair-runtime"), { method: "POST" }),
  sessions: (host: string) => request<Session[]>(runtimePath(host, "sessions")),
  directories: (host: string, path: string, prefix: string, hidden: boolean, signal: AbortSignal) => request<DirectoryListing>(hostPath(host, `directories?${new URLSearchParams({ path, prefix, hidden: String(hidden) })}`), { signal: AbortSignal.any([signal, AbortSignal.timeout(10000)]) }),
  harnesses: (host: string) => request<Harness[]>(runtimePath(host, "harnesses"), { signal: AbortSignal.timeout(15000) }),
  addHost: (data: { name: string; target: string; port: number }) => request<Host>("/api/hosts", { method: "POST", body: JSON.stringify(data) }),
  connect: (host: string) => request(hostPath(host, "connect"), { method: "POST" }),
  disconnect: (host: string) => request(hostPath(host, "disconnect"), { method: "POST" }),
  forget: (host: string) => request(hostPath(host), { method: "DELETE" }),
  createSession: (host: string, data: { title: string; workspace: string; cwd: string; harness: string }) => request<Session>(runtimePath(host, "sessions"), { method: "POST", body: JSON.stringify(data) }),
  deleteSession: (host: string, session: string) => request(runtimePath(host, `sessions/${encodeURIComponent(session)}`), { method: "DELETE" }),
  acknowledgeAttention: (host: string, session: string) => request(runtimePath(host, `sessions/${encodeURIComponent(session)}/attention/ack`), { method: "POST" }),
  harnessAction: (host: string, harness: string, action: HarnessAction, viaController = false) => request<Session>((viaController ? hostPath : runtimePath)(host, `harnesses/${encodeURIComponent(harness)}/${action}`), { method: "POST", ...(viaController ? { signal: AbortSignal.timeout(190000) } : {}) }),
};

export function websocketURL(path: string) {
  const url = new URL(path, location.origin);
  url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
  return url.toString();
}

export function errorMessage(error: unknown) { return error instanceof Error ? error.name === "TimeoutError" ? "The request timed out. Check the machine’s connection and try again." : error.message : "Something went wrong. Try again."; }
