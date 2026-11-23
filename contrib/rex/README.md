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
| **Lambe: Open Run…** | Native picker over current and archived runs. Live runs come first, each row led by a status marker (`●` live, `◌` last state of a stopped daemon, `○` archived) and showing the title, workflow, phase, attempt, and turn. The picker title names the machine whose runs it lists. Choosing a run opens `lambe logs -issue ID -follow` in a tab whose window label is `Lambe · ID` and whose pane is titled `ID · Workflow · state`, or focuses that tab if it is already open (see [Tab titles](#tab-titles)). |
| **Lambe: Open Run by Workflow…** | Pick a workflow, then one of its runs. Skips the first step when only one workflow has runs. |
| **Lambe: Switch Host…** | Opens Rex's host picker. Rex runs the actions on the host the app is connected to, so switch to the machine running your agents, then use **Open Run…**. |
| **Lambe: Check Setup** | Runs `lambe doctor` exactly as the other actions run `lambe`. |

On Rex servers without custom UI (see [Troubleshooting](#troubleshooting)),
the picker actions open the dashboard instead. For scripts,
`rex do lambe_open_run issue=ac-123` skips the picker.

## Install

The short way, on the machine that runs the Rex server:

```sh
lambe setup rex            # dry run: prints exactly what it would do
lambe setup rex --apply    # writes lambe.lua
```

See [`lambe setup rex`](#lambe-setup-rex) below. The manual steps follow.

Do this on every machine whose Rex server should offer the actions. The
actions run on the Rex **server**, so the file goes on the host that runs the
server, not on the machine whose app you are looking at.

1. Put `lambe` where a login shell finds it (`zsh -l -c 'command -v lambe'`),
   or pin its path in step 3.
2. Copy `lambe.lua` next to Rex's `init.lua` (`~/.config/rex/` on macOS and
   Linux) and load it from `init.lua`:

   ```lua
   dofile(os.getenv("HOME") .. "/.config/rex/lambe.lua")
   ```

3. Optional: pin the binary, config, and state directory. Set
   `LAMBE_REX_BIN`, `LAMBE_REX_CONFIG`, and `LAMBE_REX_STATE_DIR` in the Rex
   server's environment, or edit `bin_override`, `override`, and
   `state_dir_override` at the top of `lambe.lua`. Use them when the daemon
   runs from a service unit with its own `LAMBE_STATE_DIR` or config. Every
   action then reads the same daemon, so the picker never disagrees with the
   dashboard.
4. Load it. Reload on the machine that runs the server; a remote reload is
   refused (`server.config_reload requires the local Unix socket`):

   ```sh
   rex config check
   rex config reload            # on the server's own machine
   rex actions | grep lambe     # seven lambe_* actions
   ```

5. Verify the setup in the server's own environment, then in a headless
   stand-in for the palette (no app, no focus change):

   ```sh
   rex do lambe_check                                   # lambe doctor, via the actions' PATH
   LAMBE_REX_LUA=~/.config/rex/lambe.lua rex do contrib/rex/check.lua
   ```

   `check.lua` loads `lambe.lua` with a stubbed `rex`, answers each picker
   with its first row, and prints `PASS` plus the rows. For a remote host,
   run `lambe_check` with `rex -S HOST do lambe_check`.

## `lambe setup rex`

`lambe setup rex` installs the `lambe.lua` that shipped with your `lambe`
binary into Rex's config directory (the directory of `$REX_CONFIG`, else
`$XDG_CONFIG_HOME/rex`, else `~/.config/rex`; override with `--dir DIR`).

- **Dry run by default.** It prints the file it would create or update, the
  `dofile` line to add to `init.lua` with a commented-out key binding
  suggestion, and the `rex config check && rex config reload` command.
- **`--apply`** writes only `lambe.lua`, atomically. The file starts with a
  `-- lambe-managed: v1 sha256=...` header. Re-running updates a managed file,
  and a byte-identical hand copy is adopted.
- **It never edits `init.lua`** and writes nothing else, in `$HOME` or anywhere.
- **It refuses to overwrite** a `lambe.lua` that is not lambe's or that you
  edited after installing (the hash in the header no longer matches), unless you
  pass `--force`.
- **`--uninstall [--apply]`** removes a managed `lambe.lua` under the same
  rules and reminds you to delete the `dofile` line yourself.

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

**The picker shows nothing, or the log says `could not start lambe` / exit
127.** Rex starts programs with a minimal `PATH` (`/usr/bin:/bin:...`), so a
`lambe` in `~/.local/bin` is invisible unless run through your login shell,
which `lambe.lua` now does. If your login shell still cannot find it, set
`LAMBE_REX_BIN` or `bin_override` to an absolute path. `rex do lambe_check`
shows what the actions see.

**Open Run… opens the dashboard instead of a picker.** The Rex server has no
custom UI (`rex.ui`). Check `rex whoami --json | grep rex_version`; custom UI
is absent in `0.1.0-preview.19` and present in `0.1.0-preview.21`, and the app
must be new enough to show prompts. Restart the server after updating Rex.

**`rex config reload` fails for a remote host.** Run it on that machine, for
example in a terminal tab on the host.

**The picker lists different runs than the dashboard.** The two read
different daemons. Pin `LAMBE_REX_CONFIG` and `LAMBE_REX_STATE_DIR` (or
`override` and `state_dir_override`) to the daemon's own values; the
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
