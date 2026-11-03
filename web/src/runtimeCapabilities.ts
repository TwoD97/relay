import type { Host, RuntimeState } from "./types";

export const localCoordinatorOnly = (host: Host | undefined, runtime: RuntimeState | undefined) => host?.id === "local" && runtime?.health?.platform === "windows";
export const canCreateSessions = (host: Host | undefined, runtime: RuntimeState | undefined) => Boolean(host && (host.id !== "local" || (runtime?.health && !runtime.healthError && !localCoordinatorOnly(host, runtime))));
