# Rex events cannot replace polling for run status

Question: can `rex events` or `rex api` replace the `lambe logs -json` poll in
`contrib/rex/lambe.lua` or `internal/surface/rex`?

## Finding

No. Rex events describe Rex: sessions, windows and their labels, blocks,
clients, and what terminals report (title, cwd, bell, child exit, OSC program
status). A Lambe run's phase, attempt, and turn live in Lambe's snapshot, and
nothing in Rex emits a change when that snapshot changes. Subscribing would
only tell us when a *Rex* object changed, never that a run advanced.

What the documented events do offer, and why it is not enough:

- `terminal.child_exited` fires when a `lambe logs -follow` tab's command ends.
  That says the viewer stopped, not what state the run is in.
- `terminal.program_status_changed` carries OSC 7501 status from a program
  running in a pane. Lambe already publishes run status that way
  (`internal/surface/rex/status.go`): one record per run (`lambe/<issue>`),
  with `blocked` for runs that need a human. It still comes only from the
  pane the daemon runs in, so it cannot title or badge the per-run output
  tabs, and Rex offers no way to read records from another terminal except
  `rex block call program_status` on that block.
- `window_label_changed` would let us detect a user rename without comparing
  labels. Comparing against the last title we set is simpler and needs no
  subscription.

The palette actions are one-shot: they run, read the snapshot once, and
return. Rex also lists `rex.on` as a warning in the config context and fully
supported only in `rex do` scripts, so a subscription inside `lambe.lua`
would need a separate long-running script, which is more moving parts than
the picker needs. `rex.every` is a timer, which is polling by another name.

The poll itself is cheap: the Lua reads `lambe logs -json -brief`, which
leaves out each run's agent output (the full snapshot can reach hundreds of
kilobytes) and adds the per-run `Attention` reason, and falls back to plain
`-json` on a lambe that predates the flag.

## Decision

Not implemented. Run tab titles refresh when the run is picked again (the
snapshot is already read then). If live titles are wanted later, the smallest
design is a `rex do` script that polls `lambe logs -json -brief` on `rex.every` and
relabels panes it owns; it would not use `rex events` for status.

Verified against `rex api list` and the published events reference, not by
running a subscription.
