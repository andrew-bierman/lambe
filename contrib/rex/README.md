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

## Install

On the machine that runs the Rex server:

```sh
lambe setup rex            # dry run: prints exactly what it would do
lambe setup rex --apply    # writes the files
```

Then add the printed `dofile(...)` line to Rex's `init.lua`, and run
`rex config check && rex config reload` on that machine (a remote reload is
refused). Verify with `rex do lambe_check`; for another host use
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
  remove, the `dofile` line to add to `init.lua`, and the `rex config check &&
  rex config reload` command.
- **`--apply`** writes atomically, modules first and the shim last. Each
  managed file starts with `-- lambe-managed: v1 plugin=N sha256=...`: the
  plugin version and a hash of the body. Re-running updates managed files and
  removes managed modules a newer version no longer ships.
- **It never edits `init.lua`.**
- **It refuses to overwrite** a managed file you edited (the hash no longer
  matches) or a foreign `lambe.lua`, and writes nothing at all in that case,
  unless you pass `--force`. Put settings in `lambe.config.lua` instead; if
  you edited the `override` variables of an older single-file `lambe.lua`,
  move them there before using `--force`.
- **`--uninstall [--apply]`** removes the managed files under the same rules,
  keeps `lambe.config.lua`, and reminds you to delete the `dofile` line.

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

When the daemon runs inside a Rex terminal, it can publish an OSC program
status badge to that pane. Rex detection only observes: Lambe never starts a
Rex server to look for one. A daemon under launchd or systemd keeps
supervising when Rex closes; open a dashboard tab to follow it.

## Troubleshooting

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
goes through `lua/lambe/host.lua`, so the stub covers the whole surface. CI
runs the suite in the `rex-lua` job; `go test ./contrib/...` runs it too when
luajit is installed.
