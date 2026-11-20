-- Lambe actions for Rex: open the dashboard, the demo, or one run.
--
-- Every live invocation in this file (dashboard, run list, run follow)
-- passes the same -config, so they all read the same daemon state. Set
-- LAMBE_REX_CONFIG in the environment, or edit `override` below, to pin
-- the actions to one explicit Lambe configuration; empty means Lambe's
-- own default config. Demo mode uses sample data.

local override = ""

-- Precedence for the configuration every live invocation passes:
-- explicit action argument, then LAMBE_REX_CONFIG, then the override
-- above, then Lambe's own default config.
local function live_args(action_config)
  local config = action_config
  if config == nil or config == "" then
    config = os.getenv("LAMBE_REX_CONFIG") or ""
  end
  if config == "" then
    config = override
  end
  if config == "" then
    return {}
  end
  return { "-config", config }
end

-- `lambe logs -json` prints the shared live snapshot as JSON: PascalCase
-- Runs and History rows embedding the progress fields (IssueID, Title,
-- Phase, Attempt, Turn). rex.exec runs it with argv only; no shell reads
-- the arguments, so issue ids need no quoting.
local function read_snapshot(action_config)
  local argv = { "lambe", "logs", "-json" }
  for _, arg in ipairs(live_args(action_config)) do argv[#argv + 1] = arg end
  local r = rex.exec(argv, { timeout = 15 })
  if not r then return nil, "lambe not found on the Rex host's PATH" end
  if not r.ok then
    local why = (r.stderr ~= "" and r.stderr) or (r.stdout ~= "" and r.stdout) or ("exit " .. tostring(r.code))
    return nil, "lambe logs -json failed: " .. why
  end
  local snapshot, err = rex.json.decode(r.stdout)
  if snapshot == nil then return nil, "lambe logs -json printed no snapshot: " .. tostring(err) end
  return snapshot
end

-- One picker row per issue: current runs first, then history, deduplicated
-- so an issue with an archived earlier attempt still opens its live view.
-- Rows stay far below rex.ui.pick's 2,000-item limit because the snapshot
-- itself bounds history.
local function picker_items(snapshot)
  local items, seen = {}, {}
  local function add(runs, archived)
    for _, run in ipairs(runs or {}) do
      local id = run.IssueID
      if id and id ~= "" and not seen[id] then
        seen[id] = true
        local state = (run.Phase ~= nil and run.Phase ~= "" and run.Phase) or "unknown"
        local summary = state .. " · attempt " .. tostring(run.Attempt or 0) .. " · turn " .. tostring(run.Turn or 0)
        if archived then summary = "archived · " .. summary end
        items[#items + 1] = {
          id = id,
          title = (run.Title ~= nil and run.Title ~= "" and run.Title) or id,
          subtitle = summary,
          detail = id,
          icon = archived and "clock" or "terminal",
        }
      end
    end
  end
  add(snapshot.Runs, false)
  add(snapshot.History, true)
  return items
end

local function open_dashboard(config)
  local command = { "lambe", "tui" }
  for _, arg in ipairs(live_args(config)) do command[#command + 1] = arg end
  rex.session.new_window{
    window_label = "Lambe dashboard",
    layout = rex.layout.block{
      flavor = "com.superlogical.terminal.shell",
      options = { command = command, interactive = true },
    },
  }
end

local function open_demo()
  rex.session.new_window{
    window_label = "Lambe demo",
    layout = rex.layout.block{
      flavor = "com.superlogical.terminal.shell",
      options = { command = { "lambe", "tui", "-demo" }, interactive = true },
    },
  }
end

local function open_run(issue, config)
  local command = { "lambe", "logs", "-issue", issue, "-follow" }
  for _, arg in ipairs(live_args(config)) do command[#command + 1] = arg end
  rex.session.new_window{
    window_label = "Lambe · " .. issue,
    layout = rex.layout.block{
      flavor = "com.superlogical.terminal.shell",
      options = { command = command, interactive = true },
    },
  }
end

rex.action{
  name = "lambe_dashboard",
  title = "Lambe: Open Dashboard",
  args = { config = "string?" },
  run = function(_, args) open_dashboard(args.config) end,
}

rex.action{
  name = "lambe_demo",
  title = "Lambe: Open Demo",
  run = function() open_demo() end,
}

if rex.ui and rex.ui.pick then
  rex.action{
    name = "lambe_launch",
    title = "Lambe: Choose Dashboard…",
    args = { mode = "string?" },
    run = function(_, args)
      local mode = args.mode
      if mode == nil then
        mode = rex.ui.pick{
          title = "Open Lambe",
          items = {
            { id = "live", title = "Your dashboard", subtitle = "Use your default Lambe configuration", icon = "terminal" },
            { id = "demo", title = "Explore the demo", subtitle = "Use sample runs without writing to Beads", icon = "star" },
          },
        }
      end
      if mode == "live" or mode == "demo" then
        if mode == "demo" then open_demo() else open_dashboard() end
      end
    end,
  }
end

rex.action{
  name = "lambe_open_run",
  title = "Lambe: Open Run…",
  description = "Search current and archived Lambe runs, then follow the chosen one",
  keywords = "lambe run issue logs history",
  args = { issue = "string?", config = "string?" },
  run = function(_, args)
    local issue = args.issue
    if issue == nil or issue == "" then
      if not (rex.ui and rex.ui.pick and rex.exec and rex.json and rex.json.decode) then
        open_dashboard(args.config)
        return
      end
      local snapshot, why = read_snapshot(args.config)
      if snapshot == nil then rex.log("warn", "Lambe: " .. why) return end
      local items = picker_items(snapshot)
      if #items == 0 then
        rex.log("warn", "Lambe: no runs yet; start `lambe run` and queue an issue first")
        return
      end
      local chosen, reason = rex.ui.pick{ title = "Open Lambe run", placeholder = "Filter runs", items = items }
      if chosen == nil then
        if reason and reason ~= "cancelled" then
          rex.log("warn", "Lambe run picker: " .. reason)
        end
        return
      end
      issue = chosen
    end
    open_run(issue, args.config)
  end,
}
