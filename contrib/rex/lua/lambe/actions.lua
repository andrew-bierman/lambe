-- The Rex actions. Action names are a public contract (key bindings,
-- `rex do`, the LambeDash app); do not rename them.
local host = require("lambe.host")
local exec = require("lambe.exec")
local errors = require("lambe.errors")
local rows = require("lambe.rows")
local snapshot_mod = require("lambe.snapshot")
local windows = require("lambe.windows")
local version = require("lambe.version")

local M = {}

local function open_dashboard(config)
  windows.new_terminal_window("Lambe dashboard", exec.terminal_argv(exec.command({ "tui" }, config, true)))
end

local function open_demo()
  windows.new_terminal_window("Lambe demo", exec.terminal_argv(exec.command({ "tui", "-demo" }, nil, false)))
end

-- The window label `Lambe · <issue>` is a contract: the LambeDash app
-- focuses an existing window with this label before opening a new one.
-- `snapshot` (optional) supplies the workflow and state for the title; with
-- none, one is read if possible (best effort), else the title is the issue id.
local function open_run(issue, config, snapshot)
  if snapshot == nil then
    local ok, read = pcall(snapshot_mod.read, config)
    if ok then snapshot = read end
  end
  local title = rows.run_title(snapshot, issue)
  local label = "Lambe · " .. issue
  if windows.focus_existing(label, title) then return end
  windows.new_terminal_window(label, exec.terminal_argv(exec.command({ "logs", "-issue", issue, "-follow" }, config, true)), title)
end

-- Without the picker, say so (log once, return the message every time so
-- the caller sees it) and open the dashboard, which can do the same job.
local warned = false
local function without_picker(config, why)
  open_dashboard(config)
  local message = "Lambe: " .. why .. "; opened the dashboard instead"
  if not warned then
    warned = true
    host.log("warn", message)
  end
  return message
end

-- Pick one run; `workflow` (optional) narrows the list and `only_attention`
-- keeps the runs that need a human. Returns the issue id and the snapshot
-- it was picked from; nil when the user cancelled; and, as a third result,
-- a message when there was nothing to pick (for only_attention) so the
-- caller can report it without failing.
local function choose_run(config, workflow, only_attention)
  local snapshot = snapshot_mod.read(config)
  local items = rows.picker_items(snapshot, workflow, only_attention)
  if #items == 0 then
    local message = rows.empty_message(snapshot, workflow, only_attention)
    if only_attention then return nil, snapshot, message end
    errors.fail(message)
  end
  local where = (snapshot.Machine ~= nil and snapshot.Machine ~= "") and (" on " .. snapshot.Machine) or ""
  local title = (only_attention and "Lambe runs that need you" or "Open Lambe run") .. where
  if workflow ~= nil and workflow ~= "" then title = title .. " · " .. workflow end
  if not only_attention then
    local n = rows.attention_count(snapshot)
    if n > 0 then title = title .. " · " .. n .. " need you" end
  end
  local chosen, reason, detail = host.pick{
    title = title,
    placeholder = only_attention and "Filter runs that need you" or "Filter runs",
    items = items,
  }
  chosen = errors.picked(chosen, reason, detail, "run picker")
  if chosen == nil then return nil end
  return chosen, snapshot
end

local function register_all()
  host.action{
    name = "lambe_dashboard",
    title = "Lambe: Open Dashboard",
    category = "Lambe",
    description = "Open the Lambe terminal dashboard for this host's daemon",
    keywords = "lambe dashboard tui agents runs queue",
    args = { config = "string?" },
    run = function(_, args) open_dashboard(args.config) end,
  }

  host.action{
    name = "lambe_demo",
    title = "Lambe: Open Demo",
    category = "Lambe",
    description = "Explore the Lambe dashboard with sample runs",
    keywords = "lambe demo sample tui",
    run = function() open_demo() end,
  }

  host.action{
    name = "lambe_launch",
    title = "Lambe: Choose Dashboard…",
    category = "Lambe",
    description = "Choose between your dashboard and the sample-run demo",
    keywords = "lambe dashboard demo choose",
    args = { mode = "string?" },
    run = function(_, args)
      local mode = args.mode
      if mode == nil then
        local ok, why = host.picker_support()
        if not ok then return without_picker(nil, why) end
        local chosen, reason, detail = host.pick{
          title = "Open Lambe",
          items = {
            { id = "live", title = "Your dashboard", subtitle = "Use your default Lambe configuration", icon = "terminal" },
            { id = "demo", title = "Explore the demo", subtitle = "Use sample runs without writing to Beads", icon = "star" },
          },
        }
        mode = errors.picked(chosen, reason, detail, "dashboard picker")
      end
      if mode == "demo" then open_demo() elseif mode == "live" then open_dashboard() end
    end,
  }

  host.action{
    name = "lambe_open_run",
    title = "Lambe: Open Run…",
    category = "Lambe",
    description = "Search current and archived Lambe runs, then follow the chosen one",
    keywords = "lambe run issue logs history archived live output follow",
    args = { issue = "string?", config = "string?", workflow = "string?" },
    run = function(_, args)
      local issue = args.issue
      if issue == nil or issue == "" then
        local ok, why = host.picker_support()
        if not ok then return without_picker(args.config, why) end
        local snapshot
        issue, snapshot = choose_run(args.config, args.workflow)
        if issue == nil then return end
        return open_run(issue, args.config, snapshot)
      end
      open_run(issue, args.config)
    end,
  }

  host.action{
    name = "lambe_needs_you",
    title = "Lambe: Needs You…",
    category = "Lambe",
    description = "List only the runs waiting on a human (gate escalation, failed gates, interrupted, retrying), then follow the chosen one",
    keywords = "lambe needs you attention blocked escalation stuck review human",
    args = { config = "string?" },
    run = function(_, args)
      local ok, why = host.picker_support()
      if not ok then return without_picker(args.config, why) end
      local issue, snapshot, message = choose_run(args.config, nil, true)
      if issue == nil then return message end
      return open_run(issue, args.config, snapshot)
    end,
  }

  host.action{
    name = "lambe_open_workflow_run",
    title = "Lambe: Open Run by Workflow…",
    category = "Lambe",
    description = "Pick a workflow, then one of its current or archived runs",
    keywords = "lambe workflow switch lane route run logs",
    args = { config = "string?" },
    run = function(_, args)
      local ok, why = host.picker_support()
      if not ok then return without_picker(args.config, why) end
      local snapshot = snapshot_mod.read(args.config)
      local workflows = rows.workflow_items(snapshot)
      local workflow
      if #workflows > 1 then
        local chosen, reason, detail = host.pick{ title = "Lambe workflow", placeholder = "Filter workflows", items = workflows }
        workflow = errors.picked(chosen, reason, detail, "workflow picker")
        if workflow == nil then return end
      end
      local issue, picked = choose_run(args.config, workflow)
      if issue ~= nil then open_run(issue, args.config, picked) end
    end,
  }

  -- Rex runs these actions on the host the app is connected to, so switching
  -- hosts first is how a run opens on the machine that is running its agent.
  -- client.host.switch is the native host picker.
  host.action{
    name = "lambe_switch_host",
    title = "Lambe: Switch Host…",
    category = "Lambe",
    description = "Switch Rex to the machine running your agents, then open its Lambe runs",
    keywords = "lambe host machine remote mini switch server",
    run = function() host.queue_client("client.host.switch") end,
  }

  -- Reports what Rex and the installed script see, then runs `lambe doctor`
  -- the way every other action runs lambe (same binary, config, state).
  host.action{
    name = "lambe_check",
    title = "Lambe: Check Setup",
    category = "Lambe",
    description = "Run lambe doctor with the same binary, config, and state the actions use",
    keywords = "lambe doctor check setup troubleshoot path",
    args = { config = "string?" },
    run = function(_, args)
      local picker, note = host.picker_support()
      local report = {
        version = version.VERSION,
        api_revision = host.api_revision(),
        pickers = picker,
        picker_note = note,
        bin = exec.bin(),
      }
      local r = exec.run(exec.command({ "doctor" }, args.config, true), 30)
      host.log(r.ok and "info" or "warn", "Lambe doctor exit " .. tostring(r.code))
      report.doctor = { ok = r.ok, code = r.code, output = r.stdout .. r.stderr }
      -- A failed check must fail the caller (exit status of `rex do`), and
      -- the message carries the whole report.
      if not r.ok then
        error(string.format("Lambe: lambe doctor failed (exit %s) using %s\n%s", tostring(r.code), report.bin, report.doctor.output), 0)
      end
      return report
    end,
  }
end

function M.register() register_all() end

return M
