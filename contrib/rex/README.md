# Lambe in Rex

Open the Lambe dashboard from the Rex command palette, find any live or
archived run, and open its output on the machine that is running the agent.
Lambe works the same without Rex; this directory only adds palette actions.

Type **Lambe** in the Rex command palette:

| Action | What it does |
| --- | --- |
| **Lambe: Open Dashboard** | Opens `lambe tui` for this host's daemon. |
| **Lambe: Open Demo** | Opens the dashboard with sample runs. |
| **Lambe: Choose Dashboard…** | Native picker for the two above. |
| **Lambe: Open Run…** | Native picker over current and archived runs, one row per issue. Runs that need you come first (`▲`), then live runs (`●`, or `◌` for the last state of a stopped daemon), then archived ones (`○`). Rows show the title, workflow, a plain-language phase, and attempt and turn only once they are above their starting value; the picker title names the machine and how many runs need you. Choosing a run opens `lambe logs -issue ID -follow` in a tab whose window label is `Lambe · ID` and whose pane is titled `ID · Workflow · state`, or focuses that tab if it is already open (see [Tab titles](#tab-titles)). With nothing to list it says why (daemon stopped, no runs yet, no runs for that workflow). |
| **Lambe: Needs You…** | The same picker restricted to runs waiting on a human: gate escalation, failed gates, interrupted, retrying or waiting to retry, or any run lambe flags with an `Attention` reason. Replies "Nothing needs you right now." when the list is empty. |
| **Lambe: Open Run by Workflow…** | Pick a workflow, then one of its runs. Skips the first step when only one workflow has runs. |
| **Lambe: Switch Host…** | Opens Rex's host picker. Rex runs the actions on the host the app is connected to, so switch to the machine running your agents, then use **Open Run…**. |
| **Lambe: Check Setup** | Runs `lambe doctor` exactly as the other actions run `lambe`. |

The picker actions need Rex **preview.21** or newer (API revision 17+). On an
older server they say so (`Lambe: the run picker needs Rex preview.21 or newer
...`) and open the dashboard instead; **Check Setup** shows the revision your
server reports. Failures (lambe not found, no runs yet, a picker that could
not show) are raised as errors with the reason, so the palette or `rex do`
shows them. Cancelling a picker is silent. For scripts,
`rex do lambe_open_run issue=ac-123` skips the picker.

## Compatibility

The plugin gates on the Rex **API revision** the server reports
(`rex api call server.version`), never on the preview name. Install it on every
Rex host; each host is judged by its own server.

| Rex preview | API revision | Dashboard, demo, check | Run output (tab or split) | Pickers (Open Run, Needs You, by Workflow) | Verified |
| --- | --- | --- | --- | --- | --- |
| preview.16 | 13 | yes | yes | no, falls back to the dashboard | schemas not captured; stub profile `p16` approximates revision 14 |
| preview.19 | 14 | yes | yes | no, falls back to the dashboard | schemas captured from a real server, stub profile `p19` |
| preview.21 | 18 | yes | yes | yes (`rex.ui` needs revision 17) | schemas captured from a real server, stub profile `p21` |

Revisions 14 and 18 accept the same arguments for every `session.*` method the
plugin calls (the captured schemas are identical); what changes at 17 is
`rex.ui`. The plugin also needs a `lambe` binary from a release that has
`lambe logs -json`; with `-brief` (much smaller, and it carries each run's
`Attention` reason) it is used automatically, and an older binary is read
through plain `-json`, where "needs you" is derived from the run's phase.

## Install

On the machine that runs the Rex server:

```sh
lambe setup rex            # dry run: prints exactly what it would do
lambe setup rex --apply    # writes the files
```

Setup also adds a small marked block to Rex's `init.lua` that loads Lambe, and
checks the result with `rex config check` before replacing the file (see
below). Then run `rex config reload` on that machine (a remote reload is
refused) and verify with `rex do lambe_check`; for another host use
`rex -S HOST do lambe_check`.

The actions run on the Rex **server**, so install on every host whose server
should offer them, not on the machine whose app you are looking at.

Settings live in `lambe.config.lua` beside `init.lua`. Setup creates it once,
with the absolute path of the `lambe` binary that ran setup, and never
overwrites it:

```lua
return {
  bin = "/Users/me/.local/bin/lambe", -- "" = `lambe` through the login shell
  config = "",                        -- -config passed to every live invocation
  state_dir = "",                     -- LAMBE_STATE_DIR of the daemon to read
  placement = "tab",                  -- "tab" or "split": where run output opens
}
```

Use `config` and `state_dir` when the daemon runs from a service unit with
its own settings, so the picker never disagrees with the dashboard.
`LAMBE_REX_BIN`, `LAMBE_REX_CONFIG`, and `LAMBE_REX_STATE_DIR` in the Rex
server's environment take precedence.

`rex do lambe_check` runs in the server's own environment and returns a report
(plugin version, API revision, whether pickers are available, the binary in
use, and `lambe doctor` output); it fails if `lambe doctor` fails. Add
`--json` for machine-readable output.

## `lambe setup rex`

`lambe setup rex` installs the Lua that shipped with your `lambe` binary into
Rex's config directory (the directory of `$REX_CONFIG`, else
`$XDG_CONFIG_HOME/rex`, else `~/.config/rex`; override with `--dir DIR`):

| File | Role |
| --- | --- |
| `lambe.lua` | A tiny managed shim: it finds the modules and loads them. If the modules are missing it keeps one `Lambe: Check Setup` action that explains. |
| `lambe/*.lua` | The managed modules (actions, row building, exec, windows, Rex adapter). |
| `lambe.config.lua` | Your settings. Created once, never overwritten, never removed. |

- **Dry run by default.** It prints each file it would create, update, or
  remove, whether `init.lua` gets its block, and the follow-up commands.
  `--json` prints the same plan as a machine-readable report (also with
  `--apply`, including the check result and any error).
- **`--apply`** writes atomically, modules first and the shim last. Each
  managed file starts with `-- lambe-managed: v1 plugin=N sha256=...`: the
  plugin version and a hash of the body. Re-running updates managed files and
  removes managed modules a newer version no longer ships.
- **`init.lua` gets one managed block**, between `-- lambe-rex: begin` and
  `-- lambe-rex: end` markers, holding the `dofile` line for `lambe.lua` (and any
  keys, below). Nothing outside the markers is touched, and the block is
  appended only if `init.lua` does not already load `lambe.lua` itself. The new
  file is written beside `init.lua` and validated with `rex config check --json`
  first; if Rex rejects it, `init.lua` stays exactly as it was and the
  complaint is shown. Without the `rex` CLI on `PATH` the block is written
  unverified and setup says so. `--no-init` skips this and prints the `dofile`
  line instead.
- **`--keys`** also binds a small key set under the prefix `cmd+shift+l`
  (`>o` open run, `>n` needs you, `>d` dashboard, `>w` by workflow), inside the
  managed block. Keys already bound in your Rex config (per `rex keymap
  --json`) are skipped and reported; a bound `cmd+shift+l` skips all of them.
  Rex's built-in client shortcuts are not in that list, so a clash with one is
  not detected. Re-running without `--keys` keeps the keys already in the
  block, and no keys are ever bound by default.
- **It refuses to overwrite** a managed file you edited (the hash no longer
  matches) or a foreign `lambe.lua`, and writes nothing at all in that case,
  unless you pass `--force`. Put settings in `lambe.config.lua` instead; if
  you edited the `override` variables of an older single-file `lambe.lua`,
  move them there before using `--force`.
- **`--uninstall [--apply]`** removes the managed files under the same rules
  and the managed block from `init.lua`, and keeps `lambe.config.lua`.

## Where output opens

By default a run opens in a tab of its own, labeled `Lambe · ID`. Set
`placement = "split"` in `lambe.config.lua`, or pass `placement=split` to
`lambe_open_run`, `lambe_needs_you`, or `lambe_open_workflow_run` (for example
`rex do lambe_open_run issue=ac-123 placement=split`), to open it as a split
beside the pane you ran the action from. A split is found again by its pane
title (`ID · Workflow · state`), so picking the run twice focuses the same
pane. If a tab for the run is already open in another session on the same
server, the action focuses it there and says which session holds it instead of
opening a duplicate. Run output stays on screen after the run finishes until
you close the tab.

## Tab titles

The window label `Lambe · ID` is how the LambeDash app and the palette find an
existing run tab, so it never changes. The pane inside is titled
`ID · Workflow · state`. When you pick a run whose tab is already open, the
title is refreshed to the run's current state, but only if the pane still has
the title Lambe last set. A pane you renamed is never touched. Lambe remembers
what it set only until Rex reloads its config; after a reload it assumes
nothing is its own and leaves titles alone until the tab is reopened. Titles
update when you pick the run again, not on their own: see
`docs/solutions/rex-events.md` for why there is no live subscription.

## Use Lambe across machines

Run Lambe on each machine that supervises work, install the actions on each
Rex host, and connect the Rex app to the host you want. Switching hosts
(**Lambe: Switch Host…** or Rex's own host switcher) changes which machine's
runs the picker lists and where the output tab opens. From a shell:

```sh
rex hosts
rex -S my-host do lambe_dashboard
rex -S my-host do lambe_open_run issue=ac-123
```

## From the LambeDash app

The native LambeDash app's **Open in Rex** button uses the same window label
(`Lambe · <issue>`): it focuses that window when it exists and otherwise opens
one that follows the run's logs. The button appears only when the `rex` CLI is
installed; without Rex the app offers Terminal and copy-command fallbacks. The
app does not need `lambe.lua` installed.

## Daemon status

When `lambe run` runs in a Rex terminal, it reports run status to Rex with
OSC 7501 (the Program Status Protocol). Rex shows it in the session picker and
on the tab headers of unfocused tabs. Each run reports under its own record,
`lambe/<issue>`, titled with the issue id, so several runs never overwrite one
another:

| Run is | Badge | Message |
| --- | --- | --- |
| working (any active phase) | `working` | `ISSUE: phase` |
| waiting to retry | `idle` | `ISSUE: waiting to retry` |
| interrupted | `blocked`, kind `question` | `ISSUE: interrupted, needs a human` |
| stopped on an escalated gate | `blocked`, kind `permission` | `ISSUE: gate escalation, needs a human` |
| stopped on a red gate, or failed | `error` | `ISSUE: gate failed` |
| stopped otherwise (done, gate green) | `done` | `ISSUE: reason` |

Rex's protocol only knows the kinds `permission`, `question` and `auth`, so
lambe's own reasons (needs-human, gate-escalated, stalled) travel in the
message. A run that is only slow (stalled) is defined but not reported yet,
because the daemon has no stall signal. When the daemon exits it clears every
`lambe/...` record. Badges are written only when the daemon's output is a
terminal: under launchd or systemd stdout is a log file and nothing is emitted.
The daemon also keeps one Rex session per agent, named `lambe-<agent>`, and
reuses it across runs and restarts.

Rex detection only observes: Lambe never starts a Rex server to look for one.
A daemon under launchd or systemd keeps supervising when Rex closes; open a
dashboard tab to follow it.

## Errors

Failures are raised with `error(msg, 0)`, so the palette, `rex do` and the CLI
all show the message. Cancelling a picker is the only silent outcome.

| Message starts with | Meaning | What to do |
| --- | --- | --- |
| `Lambe: could not start `lambe`` | The binary is not where `bin` says, or is not on the login shell's `PATH` | Set `bin` in `lambe.config.lua` or `LAMBE_REX_BIN` to an absolute path |
| `Lambe: `lambe logs -json -brief` failed (...)` | The snapshot command exited non-zero; the reason follows (exit 127 means not found, `timed out` means it took more than 15 s) | Run the same command in a terminal; check `config` and `state_dir` |
| `Lambe: `lambe logs -json ...` printed no snapshot` | The output was not JSON | Update lambe; look for a wrapper script printing extra text |
| `Lambe: the run picker needs Rex preview.21 or newer ...; opened the dashboard instead` | Server API revision is below 17 | Update Rex on that host (see Compatibility) |
| `Lambe: run picker failed (no_client)` / `(failed: ...)` / `(busy)` / `(timeout)` / `(unsupported)` | Rex could not show the prompt: no app connected, a prompt already open, no answer in time, or a client without prompt support | Connect the Rex app to that host and run the action from the palette |
| `Lambe: placement must be `tab` or `split`` | Bad `placement` setting or argument | Use `tab` or `split` |
| `Lambe: lambe doctor failed (exit N) using BIN` | `lambe doctor` reported a problem; its output follows | Fix what the output names, then rerun **Check Setup** |
| `Lambe: no runs yet; ...` / `no runs recorded and the daemon is stopped; ...` / `no runs for workflow `X` yet` | Nothing to list | Start the daemon (`lambe run`, `lambe service install`) or queue an issue |
| `Nothing needs you right now.` | Not an error; **Needs You** found no run that needs a human | none |

## Troubleshooting

### Reading `lambe_check`

`rex do lambe_check --json` (or **Lambe: Check Setup**) returns:

| Field | Healthy value | If not |
| --- | --- | --- |
| `version` | the plugin version `lambe setup rex` installed | an older number than your binary's means you should rerun `lambe setup rex --apply` |
| `api_revision` | a number; 17 or more for pickers | `nil` means the server did not answer `server.version`; restart the Rex server |
| `pickers` / `picker_note` | `true`, or `false` with the reason | the note names the missing revision |
| `bin` | the absolute path you expect | `lambe` alone means the login-shell lookup is in use; set `bin` |
| `doctor.ok`, `doctor.output` | `true` | the action fails and prints `lambe doctor`'s output; fix what it lists |

**`Lambe: could not start lambe` or exit 127.** Rex starts programs with a
minimal `PATH`, so a bare `lambe` in `~/.local/bin` is only found through the
login shell. `lambe setup rex` bakes an absolute path into `lambe.config.lua`;
if you removed it or moved the binary, set `bin` there (or
`LAMBE_REX_BIN`). `rex do lambe_check` shows what the actions see.

**Open Run… opens the dashboard and says the picker needs preview.21.** The
Rex server's API revision is below 17, where custom UI (`rex.ui`) starts
(preview.21; preview.19 is revision 14). `rex do lambe_check` prints the
revision. Update Rex, restart the server, and the app must be new enough to
show prompts.

**`rex config reload` fails for a remote host.** Run it on that machine, for
example in a terminal tab on the host.

**The picker lists different runs than the dashboard.** The two read
different daemons. Set `config` and `state_dir` in `lambe.config.lua` (or
`LAMBE_REX_CONFIG` and `LAMBE_REX_STATE_DIR`) to the daemon's own values; the
dashboard and the picker then agree.

**Rows say `daemon stopped`.** The daemon that wrote the snapshot is gone, so
those rows are its last state. Start it (`lambe run` or `lambe service
install`); history stays readable meanwhile.

**The output opened on the wrong machine.** The action ran on the host your
app is connected to. Use **Lambe: Switch Host…**, then pick the run again; the
picker title says `on <machine>`.

**Nothing named Lambe in the palette.** Run `rex config check` (it reports a
Lua error) and `rex actions | grep lambe`. Rex loads one `init.lua`; make sure
it calls `dofile` on `lambe.lua`.

**`lambe tui -demo` rejects `-config`.** The demo never takes a config; it
uses sample data.

## Developing

The Lua lives in `lua/`; `lambe setup rex` embeds and installs it. The tests
need only `luajit` (Rex runs Lua 5.1, which luajit implements):

```sh
luajit contrib/rex/test/run.lua                  # all specs
UPDATE_GOLDEN=1 luajit contrib/rex/test/run.lua  # rewrite test/golden/rows.txt
```

`test/rexstub.lua` is a strict stand-in for the `rex` table: it rejects invalid
action names, picker items, and exec arguments, models the restricted APIs
while a config loads, and exposes version profiles `p16` (API 13), `p19`
(API 14, no `rex.ui`), and `p21` (API 18). All `rex.*` access in the plugin
goes through `lua/lambe/host.lua`, so the stub covers the whole surface.
The arguments of `rex.session.*` and `rex.call` methods are checked against
schemas captured from real servers: `testdata/api/revN/METHOD.json` holds
`rex api describe METHOD --json` (name, input and output schema) for API
revision N, and `go run ./contrib/rex/internal/gen` renders them into
`test/apischema.lua`, which a Go test keeps in sync. To add a revision, capture
the methods read-only (`rex -S HOST api describe METHOD --json`) into a new
`revN/` directory and rerun the generator. CI
runs the suite in the `rex-lua` job; `go test ./contrib/...` runs it too when
luajit is installed.
