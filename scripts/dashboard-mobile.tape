# Run from the repository root after building .dev/bin/lambe-dev.
Require .dev/bin/lambe-dev
Set Shell "bash"
Set FontSize 16
Set Width 470
Set Height 620
Set Padding 18
Set Margin 12
Set MarginFill "#34304A"
Set BorderRadius 12
Set WindowBar Colorful
Set Theme "Catppuccin Mocha"
Env COLORTERM "truecolor"
Type "env -u NO_COLOR .dev/bin/lambe-dev tui -demo"
Enter
Sleep 1s
Screenshot docs/assets/dashboard-mobile.png
Sleep 2s
Type "3"
Sleep 500ms
Screenshot docs/assets/history-mobile.png
Sleep 2s
Type "q"
