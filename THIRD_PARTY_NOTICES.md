# Third-party notices

Relay's original source is licensed under Apache-2.0. Dependencies retain their
own licenses; the Apache license does not replace their terms. Lockfiles identify
the dependency versions used by this source tree.

Release bundles include complete notices collected from the dependencies:

- `THIRD_PARTY_NOTICES.txt`: the Go standard library, the union of Go dependencies
  for Linux amd64, Linux arm64, and Windows amd64, and browser production packages.
  This includes the MIT-licensed Windows named-pipe dependency `go-winio`, React,
  xterm.js, and the ISC-licensed Lucide icons (including its upstream icon notices).
- `RUST_THIRD_PARTY_NOTICES.txt`: Cargo dependencies for the packaged target,
  their licenses and exact source archive URLs, the Rust standard library's
  toolchain-supplied notices, and the Microsoft WebView2 SDK loader notices on
  Windows. The inventory may also include build-time dependencies.

Unmodified MPL-2.0-covered components include `cssparser`, `cssparser-macros`,
`dtoa-short`, `option-ext`, and `selectors`. Their Source Code Form is available
under MPL-2.0 at the exact versioned source archive URLs in the desktop notice
file. Those components' source remains under MPL-2.0; Relay's separate source
files remain under Apache-2.0. See the included MPL text and
[Mozilla's license](https://www.mozilla.org/en-US/MPL/2.0/).

Linux system libraries, including GTK and WebKitGTK, are supplied separately by
the operating system. The Windows installer obtains the Evergreen WebView2
Runtime from Microsoft when required; the runtime retains Microsoft's terms.
The native WebView2 loader distributed with the Rust bindings has its own
Microsoft license and notices, included in the desktop notice file.

Claude Code, Codex CLI, and the private Node.js runtime are downloaded from their
upstream distributions only when requested. They are not included in Relay's
release bundle and retain their upstream licenses, notices, and service terms.
Relay does not relicense those products or combine provider credentials.

## Maintaining the notice inventory

`scripts/notices.py` builds the Go/browser inventory after the browser build.
`scripts/desktop_notices.py --target TARGET` builds the desktop inventory.
Desktop packaging uses the pinned Rust toolchain's `rust-docs` component for its
standard-library copyright report. The desktop toolchain file and release CI
install that component explicitly.
Both fail if a distributed dependency has no license text. When an upstream
crate omits its license file, the checked-in `licenses/manifest.json` maps its
exact version to a reviewed, checksum-pinned upstream license copy. These copies
are used without network access during packaging. Review this mapping when
upgrading dependencies; do not replace missing text with an SPDX identifier.

The public repository does not vendor the full dependencies. Their source is
available through the lockfile registries and the exact source links in release
notices. Preserve all applicable notices when redistributing source or binaries.
