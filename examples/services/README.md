# Service units — running lambe under an init system

Two drop-in units keep the `lambe run` supervisor alive across logins and
crashes. Both are examples: adjust paths, the config flag, and the user to
your install. The lock (`~/.local/state/lambe/lambe.lock`) makes a doubled
start harmless — the second instance exits with code 3 immediately — so
`KeepAlive`/`Restart` never needs a guard.

| Unit | System | Keep-alive |
|---|---|---|
| [`com.lambe.daemon.plist`](com.lambe.daemon.plist) | macOS launchd (user agents) | `KeepAlive` = always, `RunAtLoad` |
| [`lambe.service`](lambe.service) | Linux systemd (user units) | `Restart=on-failure`, `RestartSec=5` |

## macOS (launchd)

```sh
mkdir -p ~/Library/LaunchAgents
cp com.lambe.daemon.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.lambe.daemon.plist
```

## Linux (systemd user)

```sh
mkdir -p ~/.config/systemd/user
cp lambe.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now lambe.service
loginctl enable-linger "$USER"   # keep it running after logout
```

Exit codes: `0` clean shutdown, `1` error, `3` lock held (another instance
is running — not a failure; launchd's `SuccessfulExit`/systemd's
`on-failure` treat it accordingly).

See [APPROVAL_POSTURE.md](APPROVAL_POSTURE.md) for what the daemon may do
unattended.
