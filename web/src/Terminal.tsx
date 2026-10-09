import { useEffect, useRef, useState } from "react";
import { Terminal as XTerminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { ArrowDown, ArrowUp, ClipboardPaste, Copy, CornerDownLeft, Eye, Keyboard, RotateCw } from "lucide-react";
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
  const copySelection = useRef<() => void>(() => {});
  const pasteClipboard = useRef<() => void>(() => {});
  const enabledRef = useRef(enabled);
  enabledRef.current = enabled;
  const finishedRef = useRef(finished);
  finishedRef.current = finished;
  const [link, setLink] = useState<LinkState>("connecting");
  const [note, setNote] = useState("");
  const [owner, setOwner] = useState(false);
  const [available, setAvailable] = useState(false);
  const [controlReason, setControlReason] = useState("");
  const [selected, setSelected] = useState(false);
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
    const copyText = () => {
      const text = term.getSelection();
      if (!text) return;
      const fallback = () => {
        if (disposed) return;
        // Some embedded browsers do not implement the async clipboard API.
        // Select the exact xterm text in a temporary DOM field for native copy.
        const previous = document.activeElement;
        const field = document.createElement("textarea");
        field.value = text;
        field.readOnly = true;
        field.style.cssText = "position:fixed;opacity:0;pointer-events:none;left:-10000px;top:0";
        document.body.append(field);
        field.select();
        let copied = false;
        try { copied = document.execCommand("copy"); } catch { /* Report below. */ }
        field.remove();
        if (previous instanceof HTMLElement && previous.isConnected) previous.focus({ preventScroll: true });
        if (!copied) setNote("Clipboard access was denied. Select text and use the browser's Copy command.");
      };
      if (navigator.clipboard?.writeText) void navigator.clipboard.writeText(text).catch(fallback);
      else fallback();
    };
    const pasteText = () => {
      const ws = socket.current;
      const pasteGeneration = generation;
      const pasteControlGeneration = controlGeneration;
      if (ws?.readyState !== WebSocket.OPEN || !enabledRef.current || finishedRef.current || (exclusive && !ownerRef.current)) return;
      term.focus();
      const current = () => !disposed && generation === pasteGeneration && controlGeneration === pasteControlGeneration && socket.current === ws && ws.readyState === WebSocket.OPEN && enabledRef.current && !finishedRef.current && document.activeElement === term.textarea && document.hasFocus() && (!exclusive || ownerRef.current);
      const fallback = () => {
        if (!current()) return;
        // WebKit supports native paste after explicit user activation even
        // when navigator.clipboard is unavailable. xterm owns the paste event.
        let pasted = false;
        try { pasted = document.execCommand("paste"); } catch { /* Report below. */ }
        if (!pasted) setNote("Clipboard access was denied. Use the browser's Paste command or Shift+Insert.");
      };
      if (navigator.clipboard?.readText) {
        void navigator.clipboard.readText().then((text) => {
          // Never paste delayed clipboard contents into another attach, lease,
          // or field. xterm preserves the remote bracketed-paste protocol.
          if (current()) term.paste(text);
        }).catch(fallback);
      } else fallback();
    };
    copySelection.current = copyText;
    pasteClipboard.current = pasteText;
    const selection = term.onSelectionChange(() => setSelected(term.hasSelection()));
    // Standard text clipboard shortcuts work in the desktop and browser. With
    // no selected text, Ctrl+C remains the remote interrupt. Ctrl+Alt+V sends
    // the literal Ctrl+V key when a remote application needs it.
    term.attachCustomKeyEventHandler((event) => {
      // A viewer that cannot type must still be able to tab out of the terminal.
      if (event.key === "Tab" && term.options.disableStdin) return false;
      const key = event.key.toLowerCase();
      if (key === "v" && event.ctrlKey && event.altKey && !event.metaKey && !event.shiftKey) {
        event.preventDefault();
        if (event.type === "keydown") term.input("\u0016", true);
        return false;
      }
      if (event.altKey || event.ctrlKey === event.metaKey || !["c", "v"].includes(key)) return true;
      if (key === "c" && !event.shiftKey && !event.metaKey && !term.hasSelection()) return true;
      event.preventDefault();
      if (event.type !== "keydown") return false;
      if (key === "c") copyText(); else pasteText();
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
      selection.dispose();
      const ws = socket.current;
      if (ws) { ws.onclose = null; if (exclusive && ownerRef.current && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "release" })); ws.close(); }
      socket.current = null;
      terminal.current = null;
      term.dispose();
      connect.current = () => {};
      copySelection.current = () => {};
      pasteClipboard.current = () => {};
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
      <span className="terminal-hint">{finished ? "This session has finished" : active ? exclusive && !owner ? controlReason || (available ? "Click the terminal to type" : "Another viewer is controlling this terminal") : note || "Ctrl+V paste · Ctrl+C copy selection / interrupt" : note || "Input is paused"}</span>
      <button className="text-button" disabled={!selected} onClick={() => copySelection.current()} aria-label="Copy terminal selection" title="Copy selection (Ctrl+C or Ctrl+Shift+C)"><Copy size={13} /> Copy</button>
      {!finished && <button className="text-button" disabled={!writable} onClick={() => pasteClipboard.current()} aria-label="Paste text into terminal" title="Paste text (Ctrl+V or Ctrl+Shift+V). Ctrl+Alt+V sends a literal Ctrl+V."><ClipboardPaste size={13} /> Paste</button>}
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
