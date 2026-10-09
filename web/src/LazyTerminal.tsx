import { lazy, Suspense, type ComponentProps } from "react";
import { LoaderCircle } from "lucide-react";

const TerminalView = lazy(() => import("./Terminal"));

export function Terminal(props: ComponentProps<typeof TerminalView>) {
  return <Suspense fallback={<div className="terminal-loading"><LoaderCircle className="spin" size={19} /><span>Opening terminal…</span></div>}><TerminalView {...props} /></Suspense>;
}
