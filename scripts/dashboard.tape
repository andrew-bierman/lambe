# Run from the repository root after building .dev/bin/lambe-dev.
Require .dev/bin/lambe-dev
Set Shell "bash"
Set FontSize 16
Set Width 1280
Set Height 460
Set Padding 18
Set Theme "Catppuccin Mocha"
Env COLORTERM "truecolor"
Type "env -u NO_COLOR .dev/bin/lambe-dev tui -demo"
Enter
Sleep 1s
Screenshot docs/assets/dashboard.png
Sleep 2s
Type "3"
Sleep 500ms
Screenshot docs/assets/history.png
Sleep 2s
Type "q"
