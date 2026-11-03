export function sessionWindowRoute(hostId: string, sessionId: string) {
  return `#host/${encodeURIComponent(hostId)}/session/${encodeURIComponent(sessionId)}/window`;
}

export async function openSessionWindow(hostId: string, sessionId: string) {
  const native = (window as unknown as { __TAURI_INTERNALS__?: { invoke: (command: string, args: unknown) => Promise<unknown> } }).__TAURI_INTERNALS__;
  if (native) {
    // An unknown native result must not open a duplicate fallback window.
    await native.invoke("open_session_window", { hostId, sessionId });
    return;
  }
  const url = new URL(sessionWindowRoute(hostId, sessionId), location.href);
  window.open(url.href, "_blank", "noopener,noreferrer");
}
