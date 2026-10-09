export interface Host {
  id: string;
  name: string;
  target: string;
  port: number;
  status: "disconnected" | "connecting" | "installing" | "online" | "error";
  stage: string;
  error?: string;
  setupWarning?: string;
  createdAt: string;
  runtimeOperation?: {
    status: "running" | "completed" | "error";
    stage: string;
    error?: string;
    installedVersion?: string;
    runningVersion?: string;
    restartRequired?: boolean;
  };
}

export type HarnessId = "shell" | "claude" | "codex";
export type HarnessAction = "install" | "login" | "update" | "repair";
export interface MaintenanceJob {
  id: string;
  hostId: string;
  harness: "claude" | "codex";
  action: "update" | "repair";
  status: "preparing" | "starting" | "running" | "succeeded" | "failed" | "uncertain";
  stage: string;
  createdAt: string;
  updatedAt: string;
  sessionId?: string;
  sessionCreatedAt?: string;
  error?: string;
  cleanupStatus?: "pending" | "removed" | "retained" | "uncertain";
}
export interface Session {
  id: string;
  title: string;
  workspace: string;
  cwd: string;
  harness: HarnessId;
  purpose?: HarnessAction;
  status: "running" | "exited" | "interrupted";
  createdAt: string;
  updatedAt: string;
  exitCode?: number;
  attention?: { kind: "completed" | "notification" | "permission"; source: "claude-hook" | "codex-notify"; updatedAt: string };
}

export interface Harness {
  id: HarnessId;
  name: string;
  installed: boolean;
  path?: string;
  version?: string;
  authenticated?: boolean;
  authDetail?: string;
  management?: {
    supportedActions: ("install" | "update" | "repair")[];
    source: "relay" | "external" | "missing";
    strategy: "native" | "npm";
    detail: string;
  };
}

export interface DirectoryListing {
  home: string;
  path: string;
  parent: string | null;
  directories: { name: string; path: string }[];
  truncated: boolean;
}

export interface ProjectContextReport {
  path: string;
  files: { path: string; status: "created" | "updated" | "preserved" }[];
  warnings: string[];
}

export interface RuntimeState {
  sessions: Session[];
  harnesses: Harness[];
  error?: string;
  fetchedAt?: number;
  sessionsError?: string;
  harnessesError?: string;
}

export interface FleetState {
  hosts: Host[];
  runtimes: Record<string, RuntimeState>;
  version: string;
  loading: boolean;
  error: string | null;
  unauthorized: boolean;
  maintenanceJobs?: MaintenanceJob[];
  reconnecting?: boolean;
  reconnectError?: string;
}

export type Selection = { host: string; session?: string } | null;
