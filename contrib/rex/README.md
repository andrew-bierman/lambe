# Lambe in Rex

Open the Lambe dashboard from the Rex command palette and keep run history,
agent output, and Beads blockers one action away. The dashboard can reconnect
to an existing Lambe daemon on the host where Rex opens the tab.

## Add the actions

Copy [lambe.lua](lambe.lua) next to your Rex `init.lua`. Load it from
`init.lua` with `dofile("/absolute/path/to/lambe.lua")`. Run
`rex config check` and `rex config reload`, then search for **Lambe** in
the command palette.

- **Lambe: Open Dashboard** opens `lambe tui` using Lambe's default config on
  the Rex host. Use the TUI command palette (`:`) to pick a configured workflow.
- **Lambe: Open Demo** opens an interactive dashboard with sample runs.
- **Lambe: Choose Dashboard…** uses a native Rex picker to offer those two
  destinations. It requires custom UI support on the connected server and
  client. The direct actions are available on older Rex versions.
- **Lambe: Open Run…** lists current and archived runs with a native Rex
  picker and opens `lambe logs -issue ID -follow` for the chosen one in a new
  tab. Run titles, phase, attempt, and turn appear in each row; archived runs
  are marked. Cancel the picker to open nothing. On Rex versions without
  custom UI, the action opens the terminal dashboard. For scripting, `rex do lambe_open_run issue=ac-123` skips the
  picker entirely.

The Rex host needs `lambe` on its PATH. The live dashboard uses that host's
configuration, Beads tracker, and daemon state. See the
[Rex custom UI docs](https://www.superlogical.com/rex/docs/customize/custom-ui)
for the picker requirements.

## Use Lambe across machines

Run Lambe on each machine that should supervise work. Install the Rex actions
on each host, and connect Rex to the host you want to monitor. From a shell,
list configured Rex hosts and open that host's dashboard:

```sh
rex hosts
rex -S my-host do lambe_dashboard
```

Each dashboard shows the selected host's daemon, workflows, and run history.
Rex can connect to multiple hosts. Lambe currently has a separate dashboard
for each host.

## One configuration for every live path

Each Lambe configuration has its own daemon state, so every live action must
read the same one or the picker shows a different daemon than the dashboard
opens. By default the actions use Lambe's default config. To pin all of them
(dashboard, run list, and run follow) to one explicit file, set
`LAMBE_REX_CONFIG` to that config's path in the Rex server's environment, or
set the `override` variable at the top of `lambe.lua`:

```lua
local override = "/absolute/path/to/lambe.yaml"
```

`lambe tui -demo` never takes `-config`, so the demo always uses the host's
default configuration.

## Daemon status

When the daemon runs inside a Rex terminal, it can publish an OSC program
status badge to that pane. A daemon running under launchd or systemd keeps
supervising when Rex closes. Open a dashboard tab to follow that daemon.
