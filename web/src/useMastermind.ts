import { useEffect, useRef, useState } from "react";
import { api, ApiError, errorMessage } from "./api";
import type { MastermindConfig, MastermindState } from "./types";

function newest(a: MastermindState | undefined, b: MastermindState | undefined) {
  if (b && (!Number.isSafeInteger(b.revision) || !Number.isFinite(Date.parse(b.updatedAt)))) return a;
  if (!a) return b;
  if (!b) return a;
  return b.revision > a.revision || (b.revision === a.revision && Date.parse(b.updatedAt) >= Date.parse(a.updatedAt)) ? b : a;
}

// The controller owns startup and recovery. Mounting, polling and reopening a
// window only read its state; they never issue a session creation or retry one.
export function useMastermind(snapshot: MastermindState | undefined, refresh: () => void) {
  const [accepted, setAccepted] = useState<MastermindState>();
  const [busy, setBusy] = useState(false);
  const [checking, setChecking] = useState(false);
  const [error, setError] = useState<string>();
  const [unknownRevision, setUnknownRevision] = useState<number>();
  const inFlight = useRef(false);
  const mounted = useRef(true);
  const current = newest(accepted, snapshot);
  const latest = useRef(current);
  latest.current = current;
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  useEffect(() => {
    if (unknownRevision !== undefined && current && current.revision > unknownRevision) {
      setUnknownRevision(undefined);
      setError(undefined);
    }
  }, [current, unknownRevision]);

  const check = async () => {
    if (checking) return;
    setChecking(true);
    try {
      const next = await api.mastermind();
      if (mounted.current) setAccepted((old) => newest(old, next));
      refresh();
    } catch (cause) { if (mounted.current) setError(errorMessage(cause)); }
    finally { if (mounted.current) setChecking(false); }
  };
  const mutate = async (revision: number, send: () => Promise<MastermindState>) => {
    if (inFlight.current || unknownRevision !== undefined) return false;
    if (latest.current?.revision !== revision) {
      setError("Mastermind settings changed. Close and reopen settings to review the current configuration.");
      refresh();
      return false;
    }
    inFlight.current = true;
    setBusy(true); setError(undefined);
    try {
      const next = await send();
      if (mounted.current) setAccepted((old) => newest(old, next));
      refresh();
      return true;
    } catch (cause) {
      if (mounted.current) {
        const rejected = cause instanceof ApiError && cause.status >= 400 && cause.status < 500;
        setError(errorMessage(cause) + (cause instanceof ApiError && cause.status === 409 ? " Close and reopen settings to review the current configuration." : ""));
        if (!rejected) setUnknownRevision(revision);
      }
      refresh();
      return false;
    } finally {
      inFlight.current = false;
      if (mounted.current) setBusy(false);
    }
  };

  return {
    state: current, busy, checking, error, uncertain: unknownRevision !== undefined,
    clearError: () => { if (unknownRevision === undefined) setError(undefined); },
    check,
    configure: (revision: number, config: MastermindConfig) => mutate(revision, () => api.configureMastermind(revision, config)),
    start: (revision: number) => mutate(revision, () => api.startMastermind(revision)),
    disable: (revision: number) => mutate(revision, () => api.disableMastermind(revision)),
  };
}

export type MastermindActions = ReturnType<typeof useMastermind>;
