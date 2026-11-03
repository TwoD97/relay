import { useCallback, useEffect, useRef, useState } from "react";
import { api, ApiError, errorMessage } from "./api";
import type { MastermindWorkspaceMutation, MastermindWorkspaceState } from "./types";

type Intent = { revision: number; id?: string; kind?: "page" | "task" };
const valid = (value: MastermindWorkspaceState) => value && Number.isSafeInteger(value.revision) && value.revision >= 0 && Array.isArray(value.pages) && value.pages.length <= 16 && Array.isArray(value.tasks) && value.tasks.length <= 200;
const intentKey = "relay.mastermind.workspace.intent.v1";
function readIntent(): Intent | undefined {
  try {
    const value = JSON.parse(sessionStorage.getItem(intentKey) ?? "null");
    if (Number.isSafeInteger(value) && value >= 0) return { revision: value };
    if (value && Number.isSafeInteger(value.revision) && value.revision >= 0 && (!value.id || /^[a-f0-9]{32}$/.test(value.id)) && (!value.kind || ["page", "task"].includes(value.kind))) return value;
  } catch { /* No usable intent metadata. */ }
}
function saveIntent(value?: Intent) { try { if (value === undefined) sessionStorage.removeItem(intentKey); else sessionStorage.setItem(intentKey, JSON.stringify(value)); } catch { /* In-memory protection remains active if storage is unavailable. */ } }
export function useMastermindWorkspace() {
  const [state, setState] = useState<MastermindWorkspaceState>();
  const [error, setError] = useState<string>();
  const [mutationError, setMutationError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [unknown, setUnknown] = useState<Intent | undefined>(readIntent);
  const [freshRead, setFreshRead] = useState(false);
  const readGeneration = useRef(0);
  const latest = useRef(state); latest.current = state;
  const mounted = useRef(true), reading = useRef(false), writing = useRef(false);
  const accept = useCallback((next: MastermindWorkspaceState) => {
    if (!valid(next)) throw new Error("Relay returned an invalid workspace. Its contents have not been changed.");
    if (mounted.current) setState((old) => !old || next.revision >= old.revision ? next : old);
  }, []);
  const refresh = useCallback(async () => {
    if (reading.current) return;
    reading.current = true;
    const generation = readGeneration.current;
    try {
      accept(await api.mastermindWorkspace());
      if (mounted.current) { setError(undefined); if (generation === readGeneration.current) setFreshRead(true); }
    } catch (cause) { if (mounted.current) { setError(errorMessage(cause)); setFreshRead(false); } }
    finally { reading.current = false; }
  }, [accept]);
  useEffect(() => {
    mounted.current = true;
    void refresh();
    const timer = setInterval(() => void refresh(), 2000);
    return () => { mounted.current = false; clearInterval(timer); };
  }, [refresh]);
  useEffect(() => {
    // Once another revision commits, a delayed write with this older CAS can
    // no longer commit. This does not assert which item caused that revision.
    if (unknown && state && state.revision > unknown.revision) { saveIntent(); setUnknown(undefined); }
  }, [state, unknown]);
  const mutate = async (revision: number, send: () => Promise<MastermindWorkspaceMutation>, identity?: Pick<Intent, "id" | "kind">) => {
    if (writing.current || unknown || error || latest.current?.revision !== revision) return undefined;
    const intent = { revision, ...identity };
    writing.current = true; saveIntent(intent);
    setBusy(true); setMutationError(undefined);
    try {
      const result = await send();
      if (!valid(result?.workspace) || result.workspace.revision <= revision) throw new Error("The workspace change was not confirmed.");
      accept(result.workspace); saveIntent();
      return result;
    } catch (cause) {
      const rejected = cause instanceof ApiError && cause.status >= 400 && cause.status < 500;
      if (rejected) saveIntent();
      if (mounted.current) {
        setMutationError(`${errorMessage(cause)}${rejected ? " Review the current workspace before trying again." : " The change may have completed. It has not been repeated."}`);
        if (!rejected) { readGeneration.current++; setFreshRead(false); setUnknown(intent); }
      }
      void refresh(); return undefined;
    } finally { writing.current = false; if (mounted.current) setBusy(false); }
  };
  const canResume = Boolean(unknown && state && freshRead && !error && !busy);
  const resumeEditing = () => {
    if (!canResume || writing.current) return false;
    saveIntent(); setUnknown(undefined);
    setMutationError("Editing resumed after your review. The previous write remains unconfirmed and has not been repeated.");
    return true;
  };
  return { state, error, mutationError, busy, unknown, uncertain: Boolean(unknown), canResume, resumeEditing, refresh, mutate, clearMutationError: () => { if (!unknown) setMutationError(undefined); } };
}
