import type { Page, WebSocketRoute } from "@playwright/test";
import type { Approval, ObserverState, Harness, HarnessAction, HarnessId, Host, MaintenanceJob, Session } from "../src/types";

export const local: Host = { id: "local", name: "This computer", target: "local", port: 0, status: "online", stage: "Local runtime", createdAt: "2026-10-07T08:00:00Z" };
export const remote: Host = { id: "dev", name: "Development", target: "dev@example.test", port: 22, status: "online", stage: "Connected", createdAt: "2026-10-07T08:00:00Z" };
export const work: Session = { id: "session-1", title: "Build the dashboard", workspace: "Relay", cwd: "/home/dev/projects/relay", harness: "shell", status: "running", createdAt: "2026-10-07T08:00:00Z", updatedAt: "2026-10-07T08:00:00Z" };

export async function mockFleet(page: Page, options: { hosts?: Host[]; sessions?: Record<string, Session[]>; maintenanceJobs?: MaintenanceJob[]; approvals?: Record<string, Approval[]>; observers?: Record<string, ObserverState>; unauthorized?: boolean; hangHarnesses?: string[]; failHosts?: string[]; replayQueriesBeforeClaim?: boolean; autoReconnect?: boolean; deferSetupAttachments?: number; legacyRuntime?: boolean; terminalOccupied?: boolean; terminalOutput?: string; deferTerminalControl?: boolean; deferTerminalClaim?: boolean } = {}) {
  const hosts = options.hosts ?? [structuredClone(local), structuredClone(remote)];
  const sessions = options.sessions ?? { local: [], dev: [structuredClone(work)] };
  const maintenanceJobs = options.maintenanceJobs ?? [];
  const approvals = options.approvals ?? {};
  const observers = options.observers ?? {};
  const mutations: { method: string; path: string; body: unknown; csrf: string | undefined }[] = [];
  const terminalMessages: { path: string; type: string; data?: string; cols?: number; rows?: number }[] = [];
  const attachments: string[] = [];
  const directoryRequests: { host: string; path: string; prefix: string; hidden: boolean }[] = [];
  const reconnectRequests: { csrf: string | undefined }[] = [];
  const sessionRequests: Record<string, number> = {};
  const terminals = new Set<{ ws: WebSocketRoute; owner: boolean }>();
  let terminalOccupied = options.terminalOccupied ?? false;
  let deferredSetup = options.deferSetupAttachments ?? 0;
  let stateRequests = 0;
  let authorized = !options.unauthorized;
  let csrfToken = "fixture-csrf";
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    const method = request.method();
    const body = request.postData() ? request.postDataJSON() : undefined;
    // Startup reconnect is recorded separately from explicit user mutations.
    if (method !== "GET" && path !== "/api/reconnect") mutations.push({ method, path, body, csrf: request.headers()["x-relay-csrf"] });
    const json = (value: unknown, status = 200) => route.fulfill({ status, json: value });
    if (!authorized) return json({ error: "Unauthorized" }, 401);
    if (method !== "GET" && request.headers()["x-relay-csrf"] !== csrfToken) return json({ error: "Invalid CSRF token" }, 403);
    if (path === "/api/bootstrap") return json({ csrf: csrfToken, version: "0.1.0-test" });
    if (path === "/api/reconnect" && method === "POST") {
      reconnectRequests.push({ csrf: request.headers()["x-relay-csrf"] });
      let queued = 0;
      if (options.autoReconnect) for (const host of hosts) {
        if (host.id !== "local" && (host.status === "disconnected" || host.status === "error")) {
          host.status = "connecting"; host.stage = "Waiting for another host setup"; host.error = undefined; queued++;
        }
      }
      return json({ queued }, 202);
    }
    if (path === "/api/state") { stateRequests++; return json({ version: "0.1.0-test", hosts, maintenanceJobs }); }
    if (path === "/api/hosts" && method === "POST") {
      const host: Host = { ...body, id: "new-host", status: "connecting", stage: "Authenticating with SSH", createdAt: new Date().toISOString() };
      hosts.push(host); sessions[host.id] = []; return json(host, 201);
    }
    const [, , , hostId, ...rest] = path.split("/");
    const suffix = rest.join("/");
    const host = hosts.find((host) => host.id === hostId);
    if (suffix === "connect") { if (host) { host.status = "connecting"; host.stage = "Authenticating with SSH"; } return json({ ok: true }); }
    if (suffix === "disconnect") { if (host) host.status = "disconnected"; return json({ ok: true }); }
    if (suffix === "repair-runtime" && method === "POST") {
      if (!host || host.id === "local" || host.status !== "online") return json({ error: "Reconnect the remote machine before repairing its runtime." }, 409);
      host.runtimeOperation = { status: "running", stage: "Checking installed runtime" };
      return json(host, 202);
    }
    if (!suffix && method === "DELETE") { const i = hosts.findIndex((host) => host.id === hostId); if (i >= 0) hosts.splice(i, 1); return route.fulfill({ status: 204 }); }
    if (/^harnesses\/(claude|codex)\/(update|repair)$/.test(suffix) && method === "POST") {
      const harness = rest[1] as "claude" | "codex";
      const action = rest[2] as "update" | "repair";
      if (maintenanceJobs.some((job) => job.hostId === hostId && job.harness === harness && ["preparing", "starting", "running"].includes(job.status))) return json({ error: "Agent maintenance is already running." }, 409);
      const name = harness === "claude" ? "Claude Code" : "Codex";
      const now = new Date().toISOString();
      const job: MaintenanceJob = { id: `maintenance-${maintenanceJobs.length + 1}`, hostId, harness, action, status: "preparing", stage: `Preparing ${name} ${action}`, createdAt: now, updatedAt: now };
      maintenanceJobs.push(job);
      return json(job, 202);
    }
    if (options.failHosts?.includes(hostId)) return json({ error: "SSH connection unavailable" }, 503);
    if (suffix === "project-context" && method === "POST") return json({ path: body.path, files: [], warnings: [] });
    if (suffix === "directories") {
      const query = new URL(request.url()).searchParams;
      const rawPath = query.get("path") || "";
      const prefix = query.get("prefix") || "";
      const hidden = query.get("hidden") === "true";
      directoryRequests.push({ host: hostId, path: rawPath, prefix, hidden });
      const home = `/home/${hostId === "local" ? "local" : "dev"}`;
      const folder = (rawPath === "" || rawPath === "~" ? home : rawPath.startsWith("~/") ? home + rawPath.slice(1) : rawPath).replace(/\/+$/, "") || "/";
      const tree: Record<string, string[]> = {
        "/": ["home", "srv"], "/home": ["dev", "local"], "/srv": [],
        [home]: [".config", "empty", "projects", "space folder "],
        [`${home}/.config`]: [], [`${home}/empty`]: [], [`${home}/space folder `]: [],
        [`${home}/projects`]: ["api service", "relay"],
        [`${home}/projects/api service`]: ["$literal [draft]", "client & server", "src"],
        [`${home}/projects/api service/src`]: [], [`${home}/projects/api service/$literal [draft]`]: [],
        [`${home}/projects/api service/client & server`]: [], [`${home}/projects/relay`]: ["src"],
        [`${home}/projects/relay/src`]: [],
      };
      if (!folder.startsWith("/")) return json({ error: "Use an absolute path or ~/ for your home folder." }, 400);
      if (!tree[folder]) return json({ error: "This folder does not exist." }, 404);
      const directories = tree[folder].filter((name) => name.startsWith(prefix) && (hidden || prefix.startsWith(".") || !name.startsWith("."))).map((name) => ({ name, path: (folder === "/" ? "" : folder) + "/" + name }));
      return json({ home, path: folder, parent: folder === "/" ? null : folder.slice(0, folder.lastIndexOf("/")) || "/", directories, truncated: false });
    }
    if (suffix === "runtime/approvals" && hostId in approvals) return json({ requests: approvals[hostId] });
    if (/^runtime\/approvals\/[^/]+\/decision$/.test(suffix)) {
      const approval = approvals[hostId]?.find((item) => item.id === rest[2]);
      if (!approval || approval.status !== "pending" || approval.sessionId !== body.sessionId || approval.sessionCreatedAt !== body.sessionCreatedAt || Date.parse(approval.expiresAt) <= Date.now()) return json({ error: "Approval is stale or resolved." }, 409);
      Object.assign(approval, { status: "submitted", decision: body.decision });
      return json(approval);
    }
    if (suffix === "runtime/observer" && hostId in observers) return json(observers[hostId]);
    if (suffix === "runtime/observer/config" && hostId in observers) { observers[hostId].config = body; return json(observers[hostId]); }
    if (suffix === "runtime/observer/refresh" && hostId in observers) {
      const observer = observers[hostId];
      if (!observer.config.enabled) return json({ error: "Enable summaries first." }, 409);
      const session = sessions[hostId]?.find((item) => item.id === body.sessionId && item.createdAt === body.sessionCreatedAt && item.harness !== "shell" && !item.purpose);
      if (!session) return json({ error: "Session unavailable." }, 409);
      observer.running = true; observer.activeSessionId = session.id;
      const old = observer.summaries.find((item) => item.sessionId === session.id && item.sessionCreatedAt === session.createdAt);
      observer.summaries = [...observer.summaries.filter((item) => item !== old), { sessionId: session.id, sessionCreatedAt: session.createdAt, provider: "claude", model: observer.config.model, summary: "", steps: [], nextSteps: [], blockers: [], sourceStatus: session.status, stale: false, ...old, status: "queued", updatedAt: new Date().toISOString() }];
      return json(observer, 202);
    }
    if (suffix === "runtime/harnesses") {
      if (options.hangHarnesses?.includes(hostId)) return;
      const harnesses: Harness[] = [{ id: "shell", name: "Shell", installed: true }, { id: "claude", name: "Claude Code", installed: false }, { id: "codex", name: "Codex", installed: true, version: "codex-cli 1.0.0", authenticated: true }];
      if (!options.legacyRuntime) for (const harness of harnesses.filter((h) => h.id !== "shell")) harness.management = { supportedActions: ["install", "update", "repair"], source: harness.installed ? "external" : "missing", strategy: harness.id === "claude" ? "native" : "npm", detail: "A verified Relay-managed release is used for new sessions. Existing installations and sign-in are preserved." };
      return json(harnesses);
    }
    if (suffix === "runtime/sessions") {
      if (method === "GET") { sessionRequests[hostId] = (sessionRequests[hostId] ?? 0) + 1; return json(sessions[hostId] ?? []); }
      const session: Session = { ...body, id: "created-session", status: "running", createdAt: new Date().toISOString(), updatedAt: new Date().toISOString() };
      sessions[hostId] = [...(sessions[hostId] ?? []), session]; return json(session, 201);
    }
    if (/^runtime\/sessions\/[^/]+\/attention\/ack$/.test(suffix)) { const session = sessions[hostId]?.find((session) => session.id === rest[2]); if (session) delete session.attention; return json({ ok: true }); }
    if (/^runtime\/sessions\/[^/]+$/.test(suffix) && method === "DELETE") { sessions[hostId] = (sessions[hostId] ?? []).filter((session) => session.id !== rest[2]); return route.fulfill({ status: 204 }); }
    if (/^(runtime\/)?harnesses\/[^/]+\/(install|login|update|repair)$/.test(suffix)) {
      const fallback = !suffix.startsWith("runtime/");
      const harness = rest[fallback ? 1 : 2] as HarnessId;
      const purpose = rest.at(-1) as HarnessAction;
      const name = harness === "claude" ? "Claude Code" : "Codex";
      const label = { install: "Install", login: "Sign in to", update: "Update", repair: "Repair" }[purpose];
      const session: Session = { ...work, id: "harness-job", title: `${label} ${name}`, workspace: fallback ? "Setup" : "Agent setup", harness: fallback ? "shell" : harness, ...(fallback ? {} : { purpose }) };
      sessions[hostId] = [...(sessions[hostId] ?? []), session]; return json(session, 201);
    }
    return json({ error: `No fixture for ${method} ${path}` }, 404);
  });
  await page.routeWebSocket(/\/api\/hosts\/.*\/(terminal|setup-terminal)$/, (ws) => {
    const path = new URL(ws.url()).pathname;
    attachments.push(path);
    const setup = path.endsWith("setup-terminal");
    if (setup && deferredSetup > 0) { deferredSetup--; ws.close({ code: 1013, reason: "Waiting for SSH setup" }); return; }
    const terminal = { ws, owner: false };
    if (!setup) {
      terminals.add(terminal);
      ws.onClose(() => terminals.delete(terminal));
      if (!options.deferTerminalControl) ws.send(JSON.stringify({ type: "control", owner: false, available: !terminalOccupied, cols: 120, rows: 32 }));
    }
    ws.send(Buffer.from(setup ? "SSH host fingerprint: SHA256:fixture\r\nPassword: " : (options.terminalOutput ?? "\u001b[32mConnected to Relay\u001b[0m\r\n$ ")));
    ws.onMessage((raw) => {
      const message = JSON.parse(String(raw));
      terminalMessages.push({ path, ...message });
      if (message.type === "claim") {
        if (!terminalOccupied && options.replayQueriesBeforeClaim) ws.send(Buffer.from("\u001b[6n".repeat(500)));
        if (options.deferTerminalClaim && !terminalOccupied) return;
        terminal.owner = !terminalOccupied;
        ws.send(JSON.stringify({ type: "control", owner: terminal.owner, available: false, cols: 120, rows: 32 }));
      }
      if (message.type === "release") { terminal.owner = false; ws.send(JSON.stringify({ type: "control", owner: false, available: !terminalOccupied, cols: 120, rows: 32 })); }
      if (message.type === "resize" && terminal.owner) ws.send(JSON.stringify({ type: "control", owner: true, available: false, cols: message.cols, rows: message.rows }));
      if (message.type === "input" && (setup || terminal.owner)) ws.send(Buffer.from(String(message.data).replaceAll("\u007f", "\b \b")));
    });
  });
  return {
    hosts, sessions, maintenanceJobs, approvals, observers, mutations, terminalMessages, attachments, directoryRequests, reconnectRequests, sessionRequests,
    startMaintenanceJob: (id: string) => {
      const job = maintenanceJobs.find((job) => job.id === id)!;
      const name = job.harness === "claude" ? "Claude Code" : "Codex";
      const session: Session = { ...work, id: `${job.id}-session`, title: `${job.action === "update" ? "Update" : "Repair"} ${name}`, workspace: "Setup", harness: options.legacyRuntime ? "shell" : job.harness, ...(options.legacyRuntime ? {} : { purpose: job.action }), createdAt: new Date().toISOString() };
      sessions[job.hostId] = [...(sessions[job.hostId] ?? []), session];
      Object.assign(job, { sessionId: session.id, sessionCreatedAt: session.createdAt, status: "running", stage: `${job.action === "update" ? "Updating" : "Repairing"} ${name}`, updatedAt: new Date().toISOString() });
      return session;
    },
    finishMaintenanceJob: (id: string, status: "succeeded" | "failed" | "uncertain", error?: string) => {
      const job = maintenanceJobs.find((job) => job.id === id)!;
      const session = sessions[job.hostId]?.find((session) => session.id === job.sessionId && session.createdAt === job.sessionCreatedAt);
      if (session) {
        session.status = status === "uncertain" ? "interrupted" : "exited";
        session.exitCode = status === "succeeded" ? 0 : status === "failed" ? 1 : undefined;
        if (status === "succeeded") sessions[job.hostId] = sessions[job.hostId].filter((candidate) => candidate !== session);
      }
      Object.assign(job, { status, stage: status === "succeeded" ? "Maintenance completed" : "Maintenance needs attention", cleanupStatus: status === "succeeded" ? "removed" : "retained", error, updatedAt: new Date().toISOString() });
    },
    stateRequests: () => stateRequests,
    setAuthentication: (next: boolean, token = csrfToken) => { authorized = next; csrfToken = token; },
    setTerminalOccupied: (occupied: boolean) => {
      terminalOccupied = occupied;
      for (const terminal of terminals) terminal.ws.send(JSON.stringify({ type: "control", owner: terminal.owner, available: !occupied && !terminal.owner, cols: 120, rows: 32 }));
    },
    sendTerminalOutput: (data: string) => { for (const terminal of terminals) terminal.ws.send(Buffer.from(data)); },
    grantTerminalControl: () => {
      for (const terminal of terminals) {
        terminal.owner = true;
        terminal.ws.send(JSON.stringify({ type: "control", owner: true, available: false, cols: 120, rows: 32 }));
      }
    },
    disconnectTerminals: () => {
      for (const terminal of terminals) terminal.ws.close({ code: 1011, reason: "Fixture connection dropped" });
      terminals.clear();
    },
  };
}

export async function openNavigation(page: Page) {
  const menu = page.getByRole("button", { name: "Open navigation", exact: true });
  if (await menu.isVisible()) await menu.click();
}
