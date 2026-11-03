import { useEffect, useState } from "react";
import { CircleAlert, Cpu, HardDrive, MemoryStick, RefreshCw } from "lucide-react";
import { errorMessage, hostPath, request } from "./api";
import type { Host } from "./types";
import "./MachineStats.css";

type Capacity = { totalBytes: number; availableBytes: number; usedBytes: number };
export type MachineStatsSample = {
  sampledAt: string;
  platform: "linux" | "windows";
  cpu: { usagePercent: number | null; logicalCpus: number | null; intervalSeconds: number | null; error?: string };
  memory: Capacity | null;
  storage: (Capacity & { scope: "root" | "home" | "system"; path: string })[];
  warnings: string[];
};

const finite = (value: unknown): value is number => typeof value === "number" && Number.isFinite(value) && value >= 0;
function capacity(value: unknown): value is Capacity {
  if (!value || typeof value !== "object") return false;
  const item = value as Capacity;
  return finite(item.totalBytes) && item.totalBytes > 0 && finite(item.usedBytes) && item.usedBytes <= item.totalBytes && finite(item.availableBytes) && item.availableBytes <= item.totalBytes;
}
function validSample(value: MachineStatsSample) {
  return value && typeof value.sampledAt === "string" && Number.isFinite(Date.parse(value.sampledAt)) && ["linux", "windows"].includes(value.platform) && value.cpu &&
    (value.cpu.usagePercent === null || (finite(value.cpu.usagePercent) && value.cpu.usagePercent <= 100)) &&
    (value.cpu.logicalCpus === null || (Number.isInteger(value.cpu.logicalCpus) && value.cpu.logicalCpus > 0)) &&
    (value.cpu.intervalSeconds === null || finite(value.cpu.intervalSeconds)) &&
    (value.cpu.error === undefined || typeof value.cpu.error === "string") &&
    (value.memory === null || capacity(value.memory)) && Array.isArray(value.storage) && value.storage.length <= 16 &&
    value.storage.every((item) => capacity(item) && ["root", "home", "system"].includes(item.scope) && typeof item.path === "string") &&
    Array.isArray(value.warnings) && value.warnings.every((warning) => typeof warning === "string");
}
function bytes(value: number) {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  const scale = value > 0 ? Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1) : 0;
  const amount = value / 1024 ** scale;
  return `${amount.toLocaleString(undefined, { maximumFractionDigits: scale > 0 ? 1 : 0 })} ${units[scale]}`;
}
const percentage = (value: number, total: number) => Math.max(0, Math.min(100, value / total * 100));
const diskName = (scope: string) => scope === "home" ? "Home" : scope === "system" ? "System" : "Root";
function UsageBar({ percent }: { percent: number | undefined }) {
  return <div className={`machine-stat-bar${percent !== undefined && percent >= 90 ? " is-high" : ""}`} aria-hidden="true">{percent !== undefined && <i style={{ width: `${percent}%` }} />}</div>;
}

export function MachineStats({ host }: { host: Host }) {
  const [snapshot, setSnapshot] = useState<{ hostKey: string; sample: MachineStatsSample; receivedAt: number }>();
  const [failure, setFailure] = useState<{ hostKey: string; message: string }>();
  const [loading, setLoading] = useState(false);
  const [retry, setRetry] = useState(0);
  const [visible, setVisible] = useState(() => document.visibilityState === "visible");
  const online = host.status === "online";
  const hostKey = JSON.stringify([host.id, host.createdAt, host.target, host.port]);
  useEffect(() => {
    const changed = () => setVisible(document.visibilityState === "visible");
    document.addEventListener("visibilitychange", changed);
    return () => document.removeEventListener("visibilitychange", changed);
  }, []);
  useEffect(() => {
    if (!online || !visible) { setLoading(false); return; }
    let current = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let controller: AbortController | undefined;
    const poll = async () => {
      controller = new AbortController();
      const signal = controller.signal;
      setLoading(true);
      try {
        const sample = await request<MachineStatsSample>(hostPath(host.id, "stats"), { signal: AbortSignal.any([signal, AbortSignal.timeout(8000)]) });
        if (!validSample(sample)) throw new Error("Relay returned an invalid machine resource sample.");
        if (current && !signal.aborted) { setSnapshot({ hostKey, sample, receivedAt: Date.now() }); setFailure(undefined); }
      } catch (cause) {
        if (current && !signal.aborted) setFailure({ hostKey, message: errorMessage(cause) });
      } finally {
        if (current) { setLoading(false); timer = setTimeout(() => void poll(), 5000); }
      }
    };
    void poll();
    return () => { current = false; clearTimeout(timer); controller?.abort(); };
  }, [host.id, hostKey, online, visible, retry]);
  const current = snapshot?.hostKey === hostKey ? snapshot : undefined;
  const sample = current?.sample;
  const error = failure?.hostKey === hostKey ? failure.message : undefined;
  const stale = Boolean(current && (!online || !visible || error || Date.now() - current.receivedAt > 15000));
  const cpu = sample?.cpu;
  const memory = sample?.memory;
  const storage = sample?.storage.find((item) => item.scope === "system" || item.scope === "root") ?? sample?.storage[0];
  const missing = online && loading && !sample ? "Loading…" : "Unavailable";
  const cpuValue = cpu?.error ? "Unavailable" : cpu?.usagePercent === null ? "Sampling…" : cpu?.usagePercent !== undefined ? `${cpu.usagePercent.toLocaleString(undefined, { maximumFractionDigits: 1 })}%` : missing;
  return <section className={`machine-stats${stale ? " machine-stats-stale" : ""}`} aria-label="Machine resources">
    <header className="machine-stats-heading"><h2>Machine resources</h2><span>{sample ? <>{stale ? "Last sample" : "Updated"} <time dateTime={sample.sampledAt}>{new Date(sample.sampledAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })}</time></> : online ? "Read-only system usage" : "Offline"}</span><button type="button" className="icon-button" aria-label={error ? "Retry machine resources" : "Refresh machine resources"} title={error ? "Retry machine resources" : "Refresh machine resources"} disabled={!online || !visible || loading} onClick={() => setRetry((value) => value + 1)}><RefreshCw size={14} className={loading ? "spin" : undefined} /></button></header>
    <div className="machine-stat-cards">
      <article className="machine-stat-card" aria-label="CPU usage"><h3><Cpu size={15} />CPU</h3><strong>{cpuValue}</strong><UsageBar percent={cpu?.error ? undefined : cpu?.usagePercent ?? undefined} /><p>{cpu?.error || (cpu?.usagePercent === null ? "CPU usage needs two readings." : cpu?.logicalCpus ? `${cpu.logicalCpus} logical CPUs` : "Processor usage")}</p></article>
      <article className="machine-stat-card" aria-label="RAM usage"><h3><MemoryStick size={15} />RAM</h3><strong>{memory ? bytes(memory.usedBytes) : missing}</strong><UsageBar percent={memory ? percentage(memory.usedBytes, memory.totalBytes) : undefined} /><p>{memory ? `used of ${bytes(memory.totalBytes)} · ${bytes(memory.availableBytes)} available` : loading && !sample && online ? "Used and available memory" : "Memory usage unavailable"}</p></article>
      <article className="machine-stat-card" aria-label="Storage usage"><h3><HardDrive size={15} />Storage</h3><strong>{storage ? bytes(storage.usedBytes) : missing}</strong><UsageBar percent={storage ? percentage(storage.usedBytes, storage.totalBytes) : undefined} /><p>{storage ? `${bytes(storage.availableBytes)} available of ${bytes(storage.totalBytes)}` : loading && !sample && online ? "Used and available storage" : "Storage usage unavailable"}</p>{storage && <small className="machine-storage-path" title={storage.path}>{diskName(storage.scope)} · {storage.path}</small>}</article>
    </div>
    {sample && (!online || stale) && <p className="machine-stats-note" role="status">{!online ? "Offline. Showing the last available sample." : !visible ? "Updates paused while this window is hidden." : "Showing the last available sample; current usage may differ."}</p>}
    {error && online && <p className="machine-stats-error" role="alert"><CircleAlert size={14} /><span>Could not refresh machine resources: {error}</span></p>}
    {sample && sample.warnings.length > 0 && <div className="machine-stats-warnings" role="status"><CircleAlert size={14} /><ul>{sample.warnings.map((warning, index) => <li key={index}>{warning}</li>)}</ul></div>}
    {sample && sample.storage.length > 1 && <details className="machine-storage-details"><summary>Storage locations ({sample.storage.length})</summary><p>Each row describes the filesystem containing that path. Locations can share the same storage; values are not added together.</p><div>{sample.storage.map((item) => <dl key={`${item.scope}:${item.path}`}><dt>{diskName(item.scope)} <span>{item.path}</span></dt><dd>{bytes(item.usedBytes)} used · {bytes(item.availableBytes)} available · {bytes(item.totalBytes)} total</dd></dl>)}</div></details>}
  </section>;
}
