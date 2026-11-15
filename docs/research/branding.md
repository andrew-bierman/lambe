# Naming + positioning: dino-themed Symphony-compatible agent supervisor (Nov 2026)

## 1. Theme landscape

- **Rex** (Superlogical, Mitchell Hashimoto): terminal multiplexer; *is itself a dino name* (T-rex). Free, no telemetry, OSC 7501 status protocol, `rex` CLI + JSON everywhere, agent skill. Tone: plain, engineering-first, "durable sessions". https://www.superlogical.com/rex/docs/automate/agent-skill
- **Axel** (lgalabru, axel.md): "Agent orchestration, **designed for Rex**." Short friendly name + axolotl mascot, `.md` domain, tagline-first branding, credits its lineage by name (Codex app, T3 Code, Buzz, skills.sh, Rex), README badge "Built for Rex", a "Why Rex" essay page, Product Hunt launch. Apache-2.0. https://github.com/lgalabru/axel , https://axel.md/
- The family pattern: **short lowercase word, animal mascot, `name.md` site, "designed for Rex" subordinate clause** — sibling should rhyme, not copy (this project is a supervisor, not a Rex pane app).
- Adjacent to avoid clashing with: **OpenClaw** (claw/lobster), **Gas Town / Wasteland** (Yegge ecosystem), **Orca** (stablyai/orca, 120k★ fleet runner), **cmux**, **Superset**, **Agent Orchestrator** (Untrivial-ai), **Sauria** (opensauria/sauria, dino multi-agent orchestrator), **Osaurus** (osaurus-ai), **ank** (haksolot/ank, agent coordination CLI — near the Beads space). Retired "Pigeon" is out per owner.

## 2. Candidate table

Verified via web search + crates.io API + npm registry API (2026-11-15). "Clean" = 0 hits on crates.io & npm search and no dev-tool GitHub hits found.

| Name | Vibe | Status | Evidence |
|---|---|---|---|
| **ichnite** | fossil footprint; durable evidence of work | **available** | crates 0, npm 0, no GH dev-tool hits (searches above) |
| **bonebed** | collective, ancient, durable | **available** | crates 0, npm 0; only tool.io paleo calculator |
| **laurasia** | ancient supercontinent; longevity | **available** | crates 0, npm 0 |
| **mesozo** | the era itself; "ruled 165 My" | **available** | crates: 1 unrelated (`use-geologic-time` match), npm 0 |
| **gondwa** | southern supercontinent | available (minor) | crates 0, npm 0; nearby "Gondwana" game engine https://github.com/Isthimius/Gondwana |
| **osteoderm** | armor plates under skin; watchdog | available | crates 0, npm 0; 8 letters, hard to type |
| **coprolite** | playful fossil | available | crates 1 unrelated (`coprosize`); "fossilized dung" connotation |
| **kopal** | dig | minor | npm `kopal` package exists |
| **thagomizer** | stego tail club | minor | npm `thagomizer` exists; 10 letters |
| **anky** | armored ankylosaurus | minor | adjacent `ank` coordination tool https://github.com/haksolot/ank ; npm `anky-ui`; dinosay alias https://github.com/MatteoGuadrini/dinosay |
| **saur/sauria** | lizard | **taken** | https://github.com/opensauria/sauria (multi-agent orchestrator) |
| **scute** | bony plate | **taken** | https://github.com/scute-sh/scute + https://www.npmjs.com/package/@e280/scute |
| **stego** | stegosaurus | **taken** | steganography tools: https://github.com/antoniu86/stego , https://github.com/codingconcepts/stego |
| **trackway** | dino trackway | **taken** | https://github.com/me-shaon/trackway (agent-session tool) |
| **marrow** | bone interior | **taken** | https://github.com/desikai-lab/Marrow (agent MCP backend); crates `marrow-cli` |
| **fossil** | fossil | **taken** | Fossil SCM https://fossil-scm.org |
| **amber** | preserved-in-amber | **taken** | Amber MD http://ambermd.org + many packages |
| **raptor** | fast/fierce | **taken** | SpaceX engine, parser libs, many repos |
| **osaurus** | dino suffix | **taken** | https://github.com/osaurus-ai/osaurus |
| **pangaea** | supercontinent | **taken** | crates `pangaea`; npm `@pipeworx/mcp-pangaea` etc. |

Caveat: registry/GitHub search can miss things; do a final `gh repo`/namechk pass and domain check before committing.

## 3. Shortlist (5)

1. **ichnite** (available) — a fossil footprint: the supervisor leaves durable proof of every agent run; obscure word is a feature (ownable search results), story writes itself.
2. **bonebed** (available) — where many dinosaurs lie together: many agents, one durable bed; slightly playful, very ownable.
3. **laurasia** (available) — ancient continent that lasted geologic ages: pure longevity/durability elegance.
4. **anky** (minor collision) — armored, club-tailed watchdog dinosaur; protective energy, short and easy to type; conflicts with `ank` nearby.
5. **mesozo** (available) — the era when dinosaurs ruled for 165 My; era-name rides the "longevity" positioning directly.

## 4. Positioning notes

Taglines (name-agnostic until picked):
- "**Keep your agents working while you sleep.** Ichnite — the Symphony-compatible supervisor for autonomous coding agents."
- "Ichnite: durable, multi-machine orchestration for coding agents. Dinosaurs ruled the earth for 165 million years."
- "Ichnite — every agent run leaves a fossil. Symphony-compatible supervision, on every machine you own."

Axel's "ride the Rex wave, own thing" pattern, mechanically:
1. Subordinate-clause tagline: "X, **designed for Rex**" / "Built for Rex" README badge + a "Why Rex" doc page (https://github.com/lgalabru/axel, https://axel.md/).
2. `.md` domain (axel.md) — cheap, dev-coded brand signal.
3. Credit the lineage by name in README/docs (Axel cites Codex app, T3 Code, Buzz, skills.sh, Rex) — positions you inside a canon, not competing with it.
4. Get listed in ecosystem directories: `awesome-cli-coding-agents` (https://github.com/bradAGI/awesome-cli-coding-agents) has "Harnesses & orchestration" sections; Axel/Product-Hunt-style launches amplify.
5. Be "just a program in a pane": interoperate (status via OSC 7501 / hooks), don't reimplement the surface — same posture works for a Rex-optional supervisor.
