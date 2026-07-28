# Self-hosted webfonts

The dashboard serves its own typefaces. Nothing here is fetched at runtime.

## Why

Until 2026-07-26 `css/tokens.css` opened with a remote font import against
`fonts.googleapis.com`. Three problems, in order of how much they actually
cost:

1. **Silent failure.** Opened offline, or from the tailnet with no egress,
   the request just fails and the browser falls back to system faces. The
   type stops being the type and nothing reports it — the same shape as the
   other quiet breakages this system keeps producing.
2. **A third party on the load-bearing path** of a single-owner sovereign
   cloud, contacted on every page load.
3. **A request per view** carrying referrer and IP to someone else's server.

## What's here

26 `woff2` files, ~832 KB total: Playfair Display, DM Sans and JetBrains Mono
in exactly the styles and weights `tokens.css` declares — the same subsets
Google serves, restricted to **latin + latin-ext**. The vietnamese, cyrillic,
cyrillic-ext and greek subsets were dropped; this UI never sets those glyphs,
and keeping them roughly triples the payload.

Naming is `<family>-<weight>-<style>-<subset>.woff2`, which is what
`css/fonts.css` expects.

## Licensing

All three families are SIL Open Font License 1.1, which permits
redistribution. Full texts alongside, one per family, with their real
copyright lines rather than the OFL template:

| family | copyright |
|---|---|
| Playfair Display | 2005–2023 The Playfair Project Authors |
| DM Sans | 2014 The DM Sans Project Authors |
| JetBrains Mono | 2020 The JetBrains Mono Project Authors |

`fonts/trial/` is an empty leftover from a **commercial trial** download.
Trial-licensed fonts must never be committed here — if that directory ever
has bytes in it, that's a licensing problem, not a build artifact.

## Refreshing

Re-fetch the same faces, keep the filenames, and **check the `wOF2`
signature on every file before committing**. A captive portal or an error
page downloads as a plausible-looking file of the right name and the wrong
content; the first four bytes are the cheap way to catch it:

```sh
for f in *.woff2; do head -c4 "$f" | grep -q 'wOF2' || echo "NOT A FONT: $f"; done
```
