import type { ITheme } from "@xterm/xterm";

// Keep the terminal's own palette explicit. CSS filters would also alter true
// color output from remote applications, and cannot theme selection or cursor.
const palettes: Record<"light" | "dark", ITheme> = {
  dark: {
    background: "#0d1110", foreground: "#d7e0d9",
    cursor: "#c0ee78", cursorAccent: "#0d1110",
    selectionBackground: "#47563a", selectionForeground: "#f2f9ea", selectionInactiveBackground: "#35432e",
    scrollbarSliderBackground: "#73807855", scrollbarSliderHoverBackground: "#73807888", scrollbarSliderActiveBackground: "#738078bb",
    overviewRulerBorder: "#283126",
    black: "#1c2420", red: "#f48989", green: "#b5de86", yellow: "#e8cb88",
    blue: "#94bcdd", magenta: "#c8a9d7", cyan: "#82cac1", white: "#d7e0d9",
    brightBlack: "#738078", brightRed: "#ffabab", brightGreen: "#c8eea3", brightYellow: "#f5dda5",
    brightBlue: "#b0d4f2", brightMagenta: "#e0bff0", brightCyan: "#a2e7de", brightWhite: "#f2f7f3",
  },
  light: {
    background: "#fbfcf9", foreground: "#263328",
    cursor: "#426d26", cursorAccent: "#fbfcf9",
    selectionBackground: "#c7ddb7", selectionForeground: "#20321c", selectionInactiveBackground: "#e0e9d8",
    scrollbarSliderBackground: "#68756b44", scrollbarSliderHoverBackground: "#68756b77", scrollbarSliderActiveBackground: "#68756baa",
    overviewRulerBorder: "#d5ded0",
    black: "#263328", red: "#aa3434", green: "#2f6a24", yellow: "#815b08",
    blue: "#285f94", magenta: "#804d97", cyan: "#246f72", white: "#5f6d62",
    brightBlack: "#68756b", brightRed: "#b32d36", brightGreen: "#39752a", brightYellow: "#8a6500",
    brightBlue: "#326ab0", brightMagenta: "#8c4da8", brightCyan: "#28777e", brightWhite: "#3b493f",
  },
};

export function terminalTheme(appearance: "light" | "dark"): ITheme {
  // xterm compares option objects by reference. Return a fresh value and keep
  // one terminal's runtime palette changes separate from every other terminal.
  return { ...palettes[appearance] };
}
