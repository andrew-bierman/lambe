# Service units — running lambe under an init system

Two units keep the `lambe run` supervisor alive across logins and
crashes. They are examples of what `lambe service install` generates. The lock (`~/.local/state/lambe/lambe.lock`) makes a doubled
start harmless — the second instance exits with code 3 immediately — so
`KeepAlive`/`Restart` never needs a guard.

| Unit | System | Keep-alive |
|---|---|---|
| [`com.lambe.daemon.plist`](com.lambe.daemon.plist) | macOS launchd (user agents) | `KeepAlive` = always, `RunAtLoad` |
| [`lambe.service`](lambe.service) | Linux systemd (user units) | `Restart=on-failure`, `RestartSec=5` |

## Recommended: let lambe generate the unit

```sh
lambe service install --dry-run   # preview the unit and its destination
lambe service install             # write and load it (launchd on macOS, systemd user on Linux)
lambe service status
lambe service uninstall
```

The generated unit fills in your real binary and config paths and a PATH that
finds `bd` and the agent, which service managers otherwise do not have. On
Linux also run `loginctl enable-linger "$USER"` to keep it running after logout.

## Manual install (the files in this directory)

Edit the paths first (`/Users/you`, `/usr/local/bin/lambe`, the config path).

macOS:

```sh
mkdir -p ~/Library/LaunchAgents ~/Library/Logs/lambe
cp com.lambe.daemon.plist ~/Library/LaunchAgents/
launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/com.lambe.daemon.plist
```

Linux:

```sh
mkdir -p ~/.config/systemd/user
cp lambe.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now lambe.service
```

Exit codes: `0` clean shutdown, `1` error, `3` lock held (another instance
is running — not a failure; launchd's `SuccessfulExit`/systemd's
`on-failure` treat it accordingly).

See [APPROVAL_POSTURE.md](APPROVAL_POSTURE.md) for what the daemon may do
unattended.
