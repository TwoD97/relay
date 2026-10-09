import { useEffect, useRef, useState } from "react";
import { Terminal as XTerminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { ArrowDown, ArrowUp, CornerDownLeft, Eye, Keyboard, RotateCw } from "lucide-react";
import { websocketURL } from "./api";
import "@xterm/xterm/css/xterm.css";

type LinkState = "connecting" | "connected" | "disconnected";

export default function Terminal({ path, enabled = true, autoReconnect = true, exclusive = true, finished = false, label = "Terminal" }: {
  path: string; enabled?: boolean; autoReconnect?: boolean; exclusive?: boolean; finished?: boolean; label?: string;
}) {
  const container = useRef<HTMLDivElement>(null);
  const controlButton = useRef<HTMLButtonElement>(null);
  const socket = useRef<WebSocket | null>(null);
  const terminal = useRef<XTerminal | null>(null);
  const connect = useRef<() => void>(() => {});
  const enabledRef = useRef(enabled);
  enabledRef.current = enabled;
  const finishedRef = useRef(finished);
  finishedRef.current = finished;
  const [link, setLink] = useState<LinkState>("connecting");
  const [note, setNote] = useState("");
  const [owner, setOwner] = useState(false);
  const [available, setAvailable] = useState(false);
  const [controlReason, setControlReason] = useState("");
  const ownerRef = useRef(false);
  const claimPending = useRef(false);
  const claimIntent = useRef(false);
  const controlReady = useRef(false);

  useEffect(() => {
    if (!container.current) return;
    let disposed = false;
    let attempts = 0;
    let lastResize = "";
    let pendingOutput = 0;
    let generation = 0;
    let controlGeneration = 0;
    let retry: ReturnType<typeof setTimeout> | undefined;
    const term = new XTerminal({
      cursorBlink: false, disableStdin: true, cursorStyle: "bar", fontFamily: '"SFMono-Regular", Consolas, "Liberation Mono", monospace',
      fontSize: window.innerWidth < 600 ? 12 : 13, lineHeight: 1.35, scrollback: 8000,
      theme: { background: "#0d1110", foreground: "#d7e0d9", cursor: "#c0ee78", selectionBackground: "#47563a", black: "#1c2420", brightBlack: "#738078", red: "#f48989", green: "#b5de86", yellow: "#e8cb88", blue: "#94bcdd", magenta: "#c8a9d7", cyan: "#82cac1", white: "#d7e0d9" },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(container.current);
    term.textarea?.setAttribute("aria-label", `${label} input`);
    terminal.current = term;
    // Keep terminal control sequences (including Ctrl+C and Ctrl+V) intact.
    // Explicit clipboard shortcuts use xterm's selection and paste handling.
    term.attachCustomKeyEventHandler((event) => {
      // A viewer that cannot type must still be able to tab out of the terminal.
      if (event.key === "Tab" && term.options.disableStdin) return false;
      if (!event.ctrlKey || !event.shiftKey || event.altKey || event.metaKey || !["c", "v"].includes(event.key.toLowerCase())) return true;
      event.preventDefault();
      if (event.type !== "keydown") return false;
      if (event.key.toLowerCase() === "c") {
        if (term.hasSelection() && !document.execCommand("copy")) {
          void navigator.clipboard?.writeText(term.getSelection()).catch(() => { if (!disposed) setNote("Clipboard access was denied. Use the terminal context menu to copy."); });
        }
      } else {
        const ws = socket.current;
        const pasteGeneration = generation;
        const pasteControlGeneration = controlGeneration;
        if (ws?.readyState !== WebSocket.OPEN || !enabledRef.current || finishedRef.current || (exclusive && !ownerRef.current)) return false;
        if (navigator.clipboard?.readText) {
          void navigator.clipboard.readText().then((text) => {
            // A delayed permission response must not paste into a new attach
            // or after control has been released. Never queue or retry input.
            if (!disposed && generation === pasteGeneration && controlGeneration === pasteControlGeneration && socket.current === ws && ws.readyState === WebSocket.OPEN && enabledRef.current && !finishedRef.current && document.activeElement === term.textarea && document.hasFocus() && (!exclusive || ownerRef.current)) term.paste(text);
          }).catch(() => { if (!disposed) setNote("Clipboard access was denied. Use the terminal context menu to paste."); });
        } else if (!document.execCommand("paste")) setNote("Use the terminal context menu to paste.");
      }
      return false;
    });
    const resize = () => {
      if (disposed || !container.current || container.current.clientWidth < 30 || container.current.clientHeight < 30) return;
      // Observers render at the PTY's canonical geometry. Fitting a narrow
      // viewer would reflow a shared full-screen application incorrectly.
      if (exclusive && !ownerRef.current) return;
      fit.fit();
      const ws = socket.current;
      const dimensions = `${term.cols}x${term.rows}`;
      if (ws?.readyState === WebSocket.OPEN && enabledRef.current && !finishedRef.current && (!exclusive || ownerRef.current) && lastResize !== dimensions) {
        lastResize = dimensions;
        ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
      }
    };
    const observer = new ResizeObserver(resize);
    observer.observe(container.current);
    const keyInput = term.onData((data) => {
      const ws = socket.current;
      if (ws?.readyState === WebSocket.OPEN && enabledRef.current && !finishedRef.current && (!exclusive || ownerRef.current)) ws.send(JSON.stringify({ type: "input", data }));
    });
    const attach = () => {
      clearTimeout(retry);
      if (disposed || !enabledRef.current) return;
      const currentGeneration = ++generation;
      const old = socket.current;
      if (old) { old.onclose = null; old.close(); }
      setLink("connecting");
      setNote("");
      ownerRef.current = false;
      claimPending.current = false;
      claimIntent.current = false;
      controlReady.current = false;
      lastResize = "";
      setOwner(false);
      setAvailable(false);
      const ws = new WebSocket(websocketURL(path));
      socket.current = ws;
      ws.binaryType = "arraybuffer";
      ws.onopen = () => {
        if (disposed || socket.current !== ws) return;
        attempts = 0;
        // Reset after old queued writes drain, and before this attach's replay.
        term.write("", () => {
          if (disposed || generation !== currentGeneration) return;
          term.reset();
          if (ws.readyState === WebSocket.OPEN) {
            setLink("connected");
            resize();
            if (!exclusive) term.focus();
          }
        });
      };
      ws.onmessage = (event) => {
        if (disposed || socket.current !== ws) return;
        if (event.data instanceof ArrayBuffer) {
          const bytes = new Uint8Array(event.data);
          if (pendingOutput + bytes.byteLength > 4 * 1024 * 1024) {
            ws.close(4000, "Terminal display fell behind; reconnecting for a fresh snapshot.");
            return;
          }
          pendingOutput += bytes.byteLength;
          term.write(bytes, () => { pendingOutput -= bytes.byteLength; });
        }
        else if (typeof event.data === "string") {
          if (!exclusive) { term.write(event.data); return; }
          try {
            const message = JSON.parse(event.data) as { type?: string; owner?: boolean; available?: boolean; reason?: string; cols?: number; rows?: number };
            if (message.type === "control") {
              // xterm parses writes asynchronously. Queue control/geometry
              // behind preceding output, so replayed device queries cannot
              // generate live input after an early control grant.
              term.write("", () => {
                if (disposed || generation !== currentGeneration) return;
                const owns = message.owner === true && ws.readyState === WebSocket.OPEN && !finishedRef.current;
                controlReady.current = true;
                const gained = owns && !ownerRef.current;
                if (owns !== ownerRef.current) controlGeneration++;
                ownerRef.current = owns;
                term.options.disableStdin = !owns || !enabledRef.current;
                claimPending.current = false;
                if (gained) {
                  lastResize = "";
                  // A late grant must not move focus out of a dialog or field
                  // the user reached while the claim was in flight.
                  if (document.hasFocus() && (document.activeElement === term.textarea || document.activeElement === controlButton.current)) term.focus();
                }
                setOwner(owns);
                setAvailable(message.available === true);
                setControlReason(message.reason || "");
                if (owns) resize();
                else if (Number.isInteger(message.cols) && Number.isInteger(message.rows) && message.cols! >= 1 && message.cols! <= 1000 && message.rows! >= 1 && message.rows! <= 1000) term.resize(message.cols!, message.rows!);
                // A deliberate click during initial replay can precede the
                // first availability frame. Retain that intent, never text.
                if (owns || message.available !== true) claimIntent.current = false;
                else if (claimIntent.current && !claimPending.current && enabledRef.current && !finishedRef.current && ws.readyState === WebSocket.OPEN && document.activeElement === term.textarea) {
                  claimIntent.current = false;
                  claimPending.current = true;
                  ws.send(JSON.stringify({ type: "claim" }));
                }
              });
            }
          } catch { /* Runtime metadata is JSON; terminal output is binary. */ }
        }
      };
      ws.onclose = (event) => {
        if (disposed || socket.current !== ws) return;
        socket.current = null;
        ownerRef.current = false;
        claimPending.current = false;
        claimIntent.current = false;
        controlReady.current = false;
        setOwner(false);
        setLink("disconnected");
        setNote(event.reason || (enabledRef.current ? "The terminal connection closed." : "The host is disconnected."));
        if (enabledRef.current && !finishedRef.current && autoReconnect && event.code !== 1000) {
          const delay = Math.min(1000 * 2 ** attempts++, 15000);
          setNote(`Connection lost. Retrying in ${Math.round(delay / 1000)}s…`);
          retry = setTimeout(attach, delay);
        }
      };
      ws.onerror = () => { /* onclose owns retry; never replay user input. */ };
    };
    connect.current = attach;
    resize();
    if (enabledRef.current) attach(); else setLink("disconnected");
    return () => {
      disposed = true;
      clearTimeout(retry);
      observer.disconnect();
      keyInput.dispose();
      const ws = socket.current;
      if (ws) { ws.onclose = null; if (exclusive && ownerRef.current && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "release" })); ws.close(); }
      socket.current = null;
      terminal.current = null;
      term.dispose();
      connect.current = () => {};
    };
  }, [path, autoReconnect, exclusive]);

  useEffect(() => {
    terminal.current?.textarea?.setAttribute("aria-label", `${label} input`);
  }, [label]);

  useEffect(() => {
    if (!enabled) {
      socket.current?.close();
      setLink("disconnected");
    } else if (!socket.current) connect.current();
  }, [enabled]);

  const send = (data: string) => {
    if (socket.current?.readyState !== WebSocket.OPEN || !enabled || finished || (exclusive && !ownerRef.current)) return false;
    socket.current.send(JSON.stringify({ type: "input", data }));
    return true;
  };
  const active = link === "connected" && enabled && !finished;
  const writable = active && (!exclusive || owner);
  const claim = () => {
    if (!exclusive || !enabled || finished || ownerRef.current || link === "disconnected") return;
    claimIntent.current = true;
    if (link === "connecting" || !controlReady.current) return;
    if (!available) { claimIntent.current = false; return; }
    if (claimPending.current || socket.current?.readyState !== WebSocket.OPEN) return;
    claimIntent.current = false;
    claimPending.current = true;
    socket.current.send(JSON.stringify({ type: "claim" }));
  };

  useEffect(() => {
    if (terminal.current) {
      terminal.current.options.disableStdin = !writable;
      terminal.current.options.cursorBlink = writable;
    }
  }, [writable]);

  return <div className="terminal-shell">
    <div className="terminal-toolbar">
      <span className={`terminal-link ${active ? "live" : ""}`}>{active && exclusive ? owner ? <Keyboard size={12} /> : <Eye size={12} /> : <span className="status-dot" />}{finished ? "Saved output" : active ? exclusive ? owner ? "Controlling" : "Watching" : "Connected" : link === "connecting" && enabled ? "Connecting…" : "Disconnected"}</span>
      <span className="terminal-hint">{finished ? "This session has finished" : active ? exclusive && !owner ? controlReason || (available ? "Click the terminal to type" : "Another viewer is controlling this terminal") : note || "Type directly · Ctrl+Shift+C / V to copy / paste" : note || "Input is paused"}</span>
      {active && exclusive && <button ref={controlButton} className="text-button" disabled={!owner && !available} onClick={() => owner ? socket.current?.send(JSON.stringify({ type: "release" })) : claim()}>{owner ? "Release control" : "Take control"}</button>}
      {!active && enabled && !finished && <button className="text-button" onClick={() => connect.current()} aria-label="Reconnect terminal"><RotateCw size={13} /> Reconnect</button>}
    </div>
    <div ref={container} className="terminal-canvas" role="region" aria-label={label} onFocus={claim} onBlur={() => { claimIntent.current = false; }} onPointerDown={(event) => { if (event.button === 0 && !finished) { claim(); terminal.current?.focus(); } }} />
    {!finished &&
      <div className="terminal-keys" aria-label="Terminal keys">
        {[{ label: "Esc", value: "\u001b" }, { label: "Tab", value: "\t" }, { label: "Ctrl C", value: "\u0003" }].map((key) => <button key={key.label} disabled={!writable} onClick={() => send(key.value)}>{key.label}</button>)}
        <button aria-label="Arrow up" disabled={!writable} onClick={() => send("\u001b[A")}><ArrowUp size={15} /></button>
        <button aria-label="Arrow down" disabled={!writable} onClick={() => send("\u001b[B")}><ArrowDown size={15} /></button>
        <button aria-label="Enter" disabled={!writable} onClick={() => send("\r")}><CornerDownLeft size={15} /></button>
      </div>}
  </div>;
}
