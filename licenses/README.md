# Supplemental upstream license texts

Some pinned Cargo crates omit a standalone license file from their registry
archive. `manifest.json` records the exact crate version, authoritative source,
and SHA-256 of each supplemental license. GitHub sources are pinned to the
commit recorded in that crate's `.cargo_vcs_info.json`. For dual MIT/Apache
packages whose archive omits both texts, the MIT license option is included.
The MPL text is from Mozilla's official version 2.0 publication.

The WebView2 native loader comes from Microsoft.Web.WebView2 SDK 1.0.3800.47.
Its x64 `WebView2LoaderStatic.lib` was byte-compared with the library bundled by
`webview2-com-sys` 0.39.1. The NuGet archive SHA-256 was
`56c9f26bdd07916a2d1949fb58a5c7e434dfa1173577dca879206050c4e718db`.
Both its license and third-party notice file are preserved verbatim.

These license files retain their upstream terms; they are not Relay source code
and are not relicensed by Relay's top-level Apache-2.0 license.
