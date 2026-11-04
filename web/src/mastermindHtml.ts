import DOMPurify from "dompurify";
import type { ResolvedAppearance } from "./appearance";
import scrollbars from "./scrollbars.css?raw";

const policy = "default-src 'none'; script-src 'none'; connect-src 'none'; img-src data:; style-src 'unsafe-inline'; font-src 'none'; media-src 'none'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'";
const tags = ["div", "span", "p", "br", "hr", "section", "article", "header", "footer", "main", "aside", "nav", "h1", "h2", "h3", "h4", "h5", "h6", "ul", "ol", "li", "dl", "dt", "dd", "blockquote", "pre", "code", "kbd", "samp", "strong", "b", "em", "i", "small", "sub", "sup", "mark", "s", "del", "ins", "time", "table", "thead", "tbody", "tfoot", "tr", "td", "th", "caption", "colgroup", "col", "figure", "figcaption", "details", "summary", "img", "style"];
const attributes = ["class", "style", "title", "alt", "width", "height", "colspan", "rowspan", "scope", "open", "start", "reversed", "value", "datetime", "src", "lang", "dir"];
const dataImage = /^data:image\/(?:png|jpeg|gif|webp);base64,[a-z0-9+/=\s]+$/i;
const purifier = DOMPurify(window);
purifier.addHook("uponSanitizeAttribute", (_node, attribute) => {
  if (attribute.attrName === "src" && !dataImage.test(attribute.attrValue)) attribute.keepAttr = false;
});

// Every preview fits its available width. Only an unbranched chain of outer
// document wrappers is widened; nested card/grid children keep their layout.
function fitPageShells(clean: string) {
  const template = window.document.createElement("template");
  template.innerHTML = clean;
  let parent: ParentNode = template.content;
  for (let depth = 0; depth < 8; depth++) {
    const children = [...parent.children].filter((element) => element.tagName !== "STYLE");
    const hasText = [...parent.childNodes].some((node) => node.nodeType === Node.TEXT_NODE && Boolean(node.textContent?.trim()));
    if (children.length !== 1 || hasText) break;
    const shell = children[0];
    if (!(shell instanceof HTMLElement) || !["DIV", "MAIN", "SECTION", "ARTICLE"].includes(shell.tagName)) break;
    for (const [property, value] of Object.entries({ width: "100%", "max-width": "none", "min-width": "0", "margin-left": "0", "margin-right": "0", "margin-inline": "0" })) shell.style.setProperty(property, value, "important");
    parent = shell;
  }
  return template.innerHTML;
}

// Generated pages are documents, never controller code. No script, native IPC,
// forms, links, embedded browsing contexts, or external asset requests are allowed.
export function mastermindHTML(html: string, appearance: ResolvedAppearance) {
  if (new TextEncoder().encode(html).length > 128 * 1024) throw new Error("This page exceeds the 128 KiB canvas limit.");
  let clean = purifier.sanitize(html, {
    ALLOWED_TAGS: tags,
    ALLOWED_ATTR: attributes,
    ALLOW_DATA_ATTR: false,
    ALLOW_ARIA_ATTR: true,
    ALLOWED_URI_REGEXP: dataImage,
    FORBID_TAGS: ["script", "iframe", "frame", "frameset", "object", "embed", "base", "meta", "link", "form", "input", "button", "select", "textarea", "a", "svg", "math", "video", "audio", "source"],
    ADD_DATA_URI_TAGS: [],
  });
  clean = fitPageShells(clean);
  const dark = appearance === "dark";
  return `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="${policy}"><meta name="referrer" content="no-referrer"><meta name="viewport" content="width=device-width,initial-scale=1"><style>:root{color-scheme:${appearance};font:14px/1.6 system-ui,sans-serif;color:${dark ? "#e6eadd" : "#263328"};background:${dark ? "#141a16" : "#fbfcf9"};--scrollbar:${dark ? "#63745a" : "#78896b"};--scrollbar-hover:${dark ? "#7f916f" : "#657a55"};--scrollbar-active:${dark ? "#a0b887" : "#526945"};--scrollbar-track:transparent}body{margin:24px;overflow-wrap:anywhere}img{max-width:100%;height:auto}table{max-width:100%;border-collapse:collapse}td,th{padding:8px;border:1px solid ${dark ? "#394438" : "#d8dfd0"}}pre{white-space:pre-wrap}*{box-sizing:border-box}${scrollbars}</style></head><body style="width:auto!important;max-width:none!important;min-width:0!important;margin:16px!important">${clean}</body></html>`;
}
