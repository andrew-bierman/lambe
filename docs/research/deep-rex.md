# Deep dive: Rex (Superlogical) as an optional supervisor surface
Evidence dossier, 2026-11-15. No recommendations, evidence only.

## 1. Canonical identity

- Rex is a terminal + multiplexer by **Superlogical** (Mitchell Hashimoto's company; creator of Ghostty/HashiCorp). Not open source at the client level; docs live at superlogical.com/rex/docs. [verified: https://www.superlogical.com/rex/docs] (fetched 2026-11-15)
- Model: server owns PTYs and durable sessions; clients (macOS app, iOS TestFlight) each run libghostty and parse raw teed PTY bytes — no shared screen model, no in-window pane tree. [verified: https://www.bitdoze.com/superlogical-rex/] (2026-11-12)
- Vocabulary: session → window → block (one program/terminal) → client. Sessions survive disconnects; server keeps them alive with no client attached. [verified: https://www.bitdoze.com/superlogical-rex/]
- Maturity: **public beta since 2026-10-05** (macOS first); by Nov 2026 macOS client, iOS TestFlight, Linux + FreeBSD + Windows(pre-alpha role-gated) servers in testers' hands. Linux/Windows desktop clients still internal. [verified: https://www.bitdoze.com/superlogical-rex/] and [verified: https://superlogical.com/rex/docs/installation.md]
- License/cost: free, no account, no telemetry; "Rex is and will always be free", clients/servers "not going to be directly monetized"; commercialization plan promised pre-stable. License of server code not stated in docs — [single-source: bitdoze summary]. Linux server ships as distro packages (deb/rpm/apk/NixOS/FreeBSD).
- Platform: macOS app is the primary client; Linux/FreeBSD/Windows as **servers** (remote hosts). A Linux desktop client does not exist yet — inference: on a Linux workstation you can run the server but need macOS/iOS to view, a real constraint for a Go daemon assuming a local GUI.

## 2. Programmatic surface

Rex's control surface is unusually rich and **officially documented**; nothing needs reverse-engineering.

- **`rex` CLI**: auto-injected on PATH in every Rex terminal with env `REX_SESSION`, `REX_BLOCK`, `REX_SERVER`. Targeting via `-s/-w/-b/-C/-S` (session/window/block/client/server). Most commands take `--json` with documented stable output and meaningful exit codes (0/1/2/3-ambiguous/4-unreachable/130). [verified: https://www.superlogical.com/rex/docs/automate/cli.md via bitdoze] 
- **Self-documenting Server API**: `rex api list`, `rex api describe METHOD` (JSON Schema input/output + examples), `rex api call [-s SESSION] METHOD JSON`. Docs come from the connected server so they always match its version. [verified: https://www.superlogical.com/rex/docs/automate/server-api.md] (fetched 2026-11-15)
- API namespaces seen: `session.*` (create/new_split/new_window/new_layer/set_label/set_block_label/view), `block.*` (close, event, method), `client.*`, plus **block methods** from block creators (e.g. `com.superlogical.terminal.format/pwd/title` — read a terminal's screen as text). [verified: server-api.md]
- `rex api list --json` emits full machine-readable schemas "for generating bindings" — designed for exactly the caller's use case. [verified: server-api.md]
- **Lua**: config in `~/.config/rex/init.lua` (Lua 5.1, full stdlib), `rex.action` custom actions (bindable, palette, runnable via `rex do`), long-running scripts with `rex.on`/`rex.wait` event handlers (terminal.pwd_changed, program exit, window close, session created), `rex.call` mapping 1:1 to server API. [verified: https://www.superlogical.com/rex/docs/reference/lua.md, automate/lua-events.md, customize/actions.md via docs index]
- **Program Status Protocol (OSC 7501)**: escape sequence for a program to report idle/working/waiting-on-user/finished/failed; Rex badges tabs. This is the sanctioned "agent state → UI" channel. [verified: https://www.superlogical.com/rex/docs/build/program-status.md]
- **Agent skill**: `rex skill setup` installs a built-in skill + session-start hook into Claude Code/Codex/OpenCode/Pi; `rex skill show` prints the guide for any other agent. [verified: https://www.superlogical.com/rex/docs/automate/agent-skill.md] (fetched 2026-11-15)
- Sending input / reading output: `rex send -s SESSION 'text'` types into a terminal; `rex capture -b LABEL --trim` prints a block's screen; `rex wait`, `rex run --wait` adopt child exit status. [verified: bitdoze + cli docs]
- Guards: client-affecting actions require **Remote Control enabled** in the app; config reload must run on the server machine; remote callers today authenticated by **network identity** (Tailscale), native auth still roadmap. [verified: bitdoze, linux/remote-access docs]
- Undocumented gaps: no published WebSocket/HTTP control endpoint. Axel's CONTRACTS.md explicitly notes "shells out to the rex CLI with --json; **no WS yet**". [verified: https://github.com/lgalabru/axel/blob/main/docs/CONTRACTS.md]

## 3. Agent-session model in Rex

- Rex has no first-class "agent" object; agents are ordinary programs in blocks. Identity of location comes from `REX_SESSION`/`REX_BLOCK` env vars + the session-start hook. [verified: agent-skill.md]
- Session lifecycle: durable on the server across client disconnects; attach/detach implicit (clients come and go); resume = reopen. **Local macOS server restarts restore layout but drop shells** (fresh shells); Linux login server keeps per-user servers alive across upgrades. [verified: bitdoze "Rough edges"] — inference: process-level supervision cannot be delegated to Rex on macOS; only session layout persists.
- Multi-machine: first-class. Linux/FreeBSD/Windows **servers** act as remote hosts; macOS server embeds Tailscale (tsnet); connections do PAM logins (login-server mode, `/etc/rex/accounts.jsonc`); `rex -S user@host ls` targets remote servers; iOS views/steers the same sessions. Community `rex-cloud` launches disposable Vercel Sandbox VMs as Rex hosts. [verified: bitdoze; linux/server-modes.md, linux/remote-access.md; github.com/vercel-labs/rex-cloud]
- "Rex sync" as such doesn't exist; the multi-machine story is *remote Rex servers over a tailnet*, plus agents (Axel) moving work via their own event logs.

## 4. Axel as integration precedent

- Axel (github.com/lgalabru/axel, Apache-2.0, Rust, 211★, created 2026-01; v2 is a Rex-native reboot) is the existence proof. [verified: https://github.com/lgalabru/axel + skillsdirectory.com] (2026-11-05)
- Integration mechanics, from source-contract docs: detect `REX_SESSION`/`REX_BLOCK`; `Rex::api_call` wraps `rex api call -s SESSION METHOD JSON`; `new_floating_terminal` uses `session.new_layer` with shell "none"; env passed by wrapping commands in `/usr/bin/env K=V`. Status mirrored to Rex; explicitly "no WS yet". [verified: docs/CONTRACTS.md]
- Design rules Axel adopted (informative for a supervisor): treats Rex as window manager ("Axel never draws windows, it asks Rex for panes"); **"structured over scraped"** — status via Claude hooks / Codex notify / ACP / OSC 7501, "never capture-pane and grep" (Axel v1 failed there); non-goal: "driving agents by typing into their terminals" (uses ACP, JSON-RPC over stdio, instead). [verified: docs/DESIGN.md]
- Axel runs agent CLIs in Rex blocks/panes, keeps its own SQLite event log, and uses Rex `host_id` for multi-machine; P2 adds remote launch over Rex/SSH hosts. [verified: docs/DESIGN.md]

## 5. Fit inputs for a Go supervisor (findings, not recommendations)

- (a) Spawn into visible sessions: yes — `rex api call session.create` with a layout, or `session.new_split/new_window/new_layer` with a command + cwd + env; sessions can be labeled for targeting. Human can watch/steer live in any attached client. [verified: server-api.md, cli.md] Axel does exactly this. [verified: axel CONTRACTS/DESIGN]
- (b) Read state/output: two tiers — structured: OSC 7501 status, agent hooks, `rex api` events (Lua `rex.on`; note events are consumed in Lua/long-running scripts, not obviously streamable from an external process without `rex do` + stdout) — [verified: program-status.md, lua-events.md]; and screen-scrape tier: `rex capture` / `com.superlogical.terminal.format` block method (returns screen text; `--trim`). [verified: server-api.md] Inference: a daemon gets lifecycle+status cheaply, full transcripts only via capture or via the agent harness's own protocols (ACP/hooks), matching Axel's stance.
- (c) Coexistence: safe by construction — external programs act through the same versioned CLI/API a user's keybindings use; `rex skill setup` even institutionalizes third-party hooks. Client-control actions gated behind Remote Control setting. [verified: agent-skill.md, actions.md] Degradation: with no `REX_SESSION`/server reachable (`rex ls` fails, exit 4), fall back to plain PTY — Axel's DESIGN shows the same "outside Rex, run inline" pattern. [verified: axel DESIGN.md]
- Minimum viable integration (inference from above evidence): (1) shell out to `rex` binary, detect via `REX_SERVER`/`rex ls`; (2) create a labeled session per agent via `session.create --json`; (3) emit OSC 7501 status from supervised agents; (4) poll `session.view` + `capture` for output; (5) `rex send` only for emergency steering. Zero linking, pure subprocess + JSON.
- What breaks on Rex change: beta product ("0.1.0" in doc examples); docs are server-versioned via `rex api describe` which mitigates drift [verified: server-api.md]; the WS-less CLI-shellout pattern, method names, and OSC 7501 are the contract points; auth model for remote servers is still roadmap (network identity only) — remote supervision over untrusted networks is not yet supported. [verified: bitdoze, remote-access]
