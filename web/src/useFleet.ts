import { useCallback, useEffect, useRef, useState } from "react";
import { api, ApiError, bootstrap, errorMessage } from "./api";
import type { FleetState, MaintenanceJob, RuntimeState } from "./types";
import { maintenanceActive } from "./MaintenanceJobs";

export function useFleet() {
  const [state, setState] = useState<FleetState>({ hosts: [], runtimes: {}, version: "", loading: true, error: null, unauthorized: false });
  const trigger = useRef<() => void>(() => {});
  const triggerReconnect = useRef<() => void>(() => {});
  const refresh = useCallback(() => trigger.current(), []);
  const reconnect = useCallback(() => triggerReconnect.current(), []);

  useEffect(() => {
    let mounted = true;
    let busy = false;
    let queued = false;
    let reconnectBusy = false;
    let reconnectToken: string | null = null;
    let timer: ReturnType<typeof setTimeout>;
    const updating = new Set<string>();
    const reconnectSaved = async () => {
      if (reconnectBusy || !mounted) return;
      reconnectBusy = true;
      setState((old) => ({ ...old, reconnecting: true, reconnectError: undefined }));
      try {
        await api.reconnect();
        if (mounted) trigger.current();
      } catch (error) {
        if (mounted) setState((old) => ({ ...old, reconnectError: errorMessage(error) }));
      } finally {
        reconnectBusy = false;
        if (mounted) setState((old) => ({ ...old, reconnecting: false }));
      }
    };
    // Each host and endpoint refreshes independently. A slow harness probe must
    // not hold up terminal session discovery or another machine's state.
    const refreshRuntime = async (host: string, kind: "sessions" | "harnesses") => {
      const key = `${host}/${kind}`;
      if (updating.has(key)) return;
      updating.add(key);
      try {
        const value = kind === "sessions" ? await api.sessions(host) : await api.harnesses(host);
        if (mounted) setState((old) => {
          const current = old.runtimes[host] ?? { sessions: [], harnesses: [] };
          const next = { ...current, [kind]: value ?? [], [`${kind}Error`]: undefined, ...(kind === "sessions" ? { fetchedAt: Date.now() } : {}) } as RuntimeState;
          next.error = [next.sessionsError, next.harnessesError].filter(Boolean).join(" · ") || undefined;
          return { ...old, runtimes: { ...old.runtimes, [host]: next } };
        });
      } catch (error) {
        if (mounted) setState((old) => {
          const current = old.runtimes[host] ?? { sessions: [], harnesses: [] };
          const next = { ...current, [`${kind}Error`]: errorMessage(error) };
          next.error = [next.sessionsError, next.harnessesError].filter(Boolean).join(" · ");
          return { ...old, runtimes: { ...old.runtimes, [host]: next } };
        });
      } finally { updating.delete(key); }
    };
    const poll = async () => {
      if (busy) { queued = true; return; }
      busy = true;
      clearTimeout(timer);
      try {
        const authenticated = await bootstrap();
        if (!mounted) return;
        // One request per controller authentication, never once per poll. Mark
        // before sending: a lost mutation reply must not replay the request.
        if (reconnectToken !== authenticated.csrf) {
          reconnectToken = authenticated.csrf;
          void reconnectSaved();
        }
        const fleet = await api.state();
        if (!mounted) return;
        setState((old) => {
          const jobs = fleet.maintenanceJobs ?? [];
          // A state read already in flight can predate a successful create
          // response. Keep its accepted job until the server snapshot catches up.
          const accepted = (old.maintenanceJobs ?? []).filter((job) => maintenanceActive(job) && !jobs.some((item) => item.id === job.id));
          return { ...old, ...fleet, hosts: fleet.hosts ?? [], maintenanceJobs: [...jobs, ...accepted], loading: false, error: null, unauthorized: false };
        });
        for (const host of (fleet.hosts ?? []).filter((host) => host.status === "online")) {
          void refreshRuntime(host.id, "sessions");
          void refreshRuntime(host.id, "harnesses");
        }
      } catch (error) {
        if (mounted) setState((old) => ({ ...old, loading: false, error: errorMessage(error), unauthorized: error instanceof ApiError && error.status === 401 }));
      } finally {
        busy = false;
        if (mounted) {
          const delay = queued ? 0 : 2000;
          queued = false;
          timer = setTimeout(poll, delay);
        }
      }
    };
    trigger.current = () => void poll();
    triggerReconnect.current = () => void reconnectSaved();
    void poll();
    return () => { mounted = false; clearTimeout(timer); trigger.current = () => {}; triggerReconnect.current = () => {}; };
  }, []);

  const upsertRuntime = useCallback((host: string, session: RuntimeState["sessions"][number]) => {
    setState((old) => {
      const current = old.runtimes[host] ?? { sessions: [], harnesses: [] };
      return { ...old, runtimes: { ...old.runtimes, [host]: { ...current, sessions: [...current.sessions.filter((s) => s.id !== session.id), session] } } };
    });
  }, []);

  const upsertMaintenance = useCallback((job: MaintenanceJob) => {
    setState((old) => ({ ...old, maintenanceJobs: [...(old.maintenanceJobs ?? []).filter((item) => item.id !== job.id), job] }));
  }, []);

  return { state, refresh, reconnect, upsertRuntime, upsertMaintenance };
}
