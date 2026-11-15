# Name-collision check — round 2 (lambe / lyre / cory)

Date: 2026-11-15. Method: npm/crates.io/PyPI/Homebrew registry APIs (direct), GitHub + web search.

## lambe — AVAILABLE (one niche collision)
- npm `lambe`: 404 (free). crates.io: 404. PyPI: 404. Homebrew core formula + cask: none.
- Only registry hit: Dart package `lambe` (Lambë, Quenya for "language") — a query
  language CLI + MCP server, active 2026, small (93 downloads). Not Go/npm/crates space.
  https://pub.dev/packages/lambe — https://github.com/hakimjonas/lambe
- Near-names on npm are unrelated audio encoders (lame, lamejs, node-lame).
- Connotations: Lambë (Tolkien); Spanish/Portuguese "lamer" = to lick (mild, low risk);
  Lambeau Field (US football, weak). No trademark found in dev-tool space.

## lyre — TAKEN (blocking)
- npm `lyre`: taken — GraphQL client for TypeScript (stale, 2022).
  https://www.npmjs.com/package/lyre
- crates.io `lyre`: taken (1,585 downloads, last update 2022).
  https://crates.io/crates/lyre
- PyPI `lyre`: taken (v0.1.4, Chris Chambers). https://pypi.org/project/lyre/
- Commercial product: **Lyre by 8Labs GmbH** — active, enterprise **AI voice-agent
  platform** (agents for phone calls). Same "autonomous agents" category; docs claim
  `brew install lyre` and `npm install -g lyre`. Direct trademark + mindshare collision.
  https://8labs.ai/en/docs/ — https://omr.com/en/reviews/product/lyre
- Verified prior knowledge: **Lyrebird** (YC S17, voice cloning) acquired by Descript
  Sep 2019, wound down; brand lives on inside Descript Overdub. 8Labs explicitly says
  "Lyre is short for lyrebird." https://www.ycombinator.com/companies/lyrebird
- GitHub: multiple repos named lyre, several dev/terminal tools: music Lisp interpreter
  + `@lyre/core` (https://github.com/amuradyan/lyre), music TUI `lyre-tui`
  (https://github.com/DeadZone-0/lyre), transcription platform
  (https://github.com/nocoo/lyre), plus `@~lyre/ai-agents` npm AI-agent SDK.
- Connotations: musical instrument (fine); lyrebird = mimicry — "imitation, not real,"
  arguably poor for an orchestrator brand.

## cory — MINOR COLLISIONS
- npm `cory`: taken but deprecated ("use next.js"), 9 years stale, ~0 weekly downloads.
  https://www.npmjs.com/package/cory — https://github.com/leo/cory (archived, 61 stars)
- crates.io `cory`: taken — panon-btc/cory, Bitcoin ancestry explorer, **active 2026**,
  installable via `cargo install cory`. Most concrete blocker (Go projects often also
  ship Rust-adjacent tooling; name squatted on the systems-CLI ecosystem).
  https://github.com/panon-btc/cory — https://crates.io/crates/cory
- PyPI `cory`: 404 (free). Homebrew: none.
- No commercial product or trademark found.
- Connotations: common English first name (Cory Doctorow, Cory Booker) — reads as a
  person, not a tool; hard to search ("cory" + anything drowns in namesakes).

## Verdict
- lambe: cleanest — all four target registries free; only a small Dart package shares it.
- cory: usable but soft — stale npm name, active Rust crate, weak searchability.
- lyre: taken on npm/crates/PyPI, and an active commercial AI-agent product owns the
  brand space. Rule out.
