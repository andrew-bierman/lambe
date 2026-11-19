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

The Rex host needs `lambe` on its PATH. The live dashboard uses that host's
configuration, Beads tracker, and daemon state. See the
[Rex custom UI docs](https://www.superlogical.com/rex/docs/customize/custom-ui)
for the picker requirements.

## Daemon status

When the daemon runs inside a Rex terminal, it can publish an OSC program
status badge to that pane. A daemon running under launchd or systemd keeps
supervising when Rex closes. Open a dashboard tab to follow that daemon.
