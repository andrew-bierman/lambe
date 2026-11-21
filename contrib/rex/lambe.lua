-- Lambe actions for Rex: open the dashboard, the demo, or one run, and
-- pick a workflow or a Rex host from the command palette.
--
-- Every live invocation in this file (dashboard, run list, run follow)
-- passes the same -config, binary, and state directory, so they all read
-- the same daemon state. Set LAMBE_REX_CONFIG, LAMBE_REX_BIN, and
-- LAMBE_REX_STATE_DIR in the Rex server's environment, or edit the
-- variables below; empty means Lambe's own defaults (`lambe` from the login
-- shell's PATH, its default config, ~/.local/state/lambe). Demo mode uses
-- sample data.

local override = ""
local bin_override = ""
local state_dir_override = ""

-- Returns the first non-empty argument. select("#") counts nil arguments,
-- which ipairs would stop at.
local function first_set(...)
  for i = 1, select("#", ...) do
    local value = select(i, ...)
    if value ~= nil and value ~= "" then return value end
  end
  return ""
end

-- Precedence for each setting: explicit action argument (config only),
-- then the environment, then the variable above.
local function lambe_bin()
  return first_set(os.getenv("LAMBE_REX_BIN"), bin_override, "lambe")
end

local function live_args(action_config)
  local config = first_set(action_config, os.getenv("LAMBE_REX_CONFIG"), override)
  if config == "" then return {} end
  return { "-config", config }
end

-- The daemon's state directory decides which snapshot `lambe logs` reads.
-- A daemon started under launchd, systemd, or a shell with LAMBE_STATE_DIR
-- set must be read with the same value, so it is forwarded through env(1).
local function lambe_argv(sub, config, live)
  local argv = {}
  local state_dir = first_set(os.getenv("LAMBE_REX_STATE_DIR"), state_dir_override)
  if live and state_dir ~= "" then
    argv = { "env", "LAMBE_STATE_DIR=" .. state_dir }
  end
  argv[#argv + 1] = lambe_bin()
  for _, arg in ipairs(sub) do argv[#argv + 1] = arg end
  if live then
    for _, arg in ipairs(live_args(config)) do argv[#argv + 1] = arg end
  end
  return argv
end

-- rex.exec starts programs with a minimal PATH (/usr/bin:/bin:...), so a
-- `lambe` in ~/.local/bin or a version-manager shim is invisible to it even
-- though terminal blocks, which use the login shell, find it. Run bare
-- names through the user's login shell; an absolute path runs directly.
local posix_shells = { sh = true, bash = true, zsh = true, dash = true, ksh = true }

local function login_shell()
  local shell = os.getenv("SHELL") or ""
  if posix_shells[shell:match("([^/]+)$") or ""] then return shell end
  return "/bin/sh"
end

local function exec_lambe(argv, timeout)
  local program = argv[1] == "env" and argv[3] or argv[1]
  if program:find("/", 1, true) then
    return rex.exec(argv, { timeout = timeout })
  end
  local wrapped = { login_shell(), "-l", "-c", 'exec "$@"', "lambe" }
  for _, arg in ipairs(argv) do wrapped[#wrapped + 1] = arg end
  return rex.exec(wrapped, { timeout = timeout })
end

-- `lambe logs -json` prints the shared live snapshot as JSON: PascalCase
-- Runs and History rows embedding the progress fields (IssueID, Title,
-- Workflow, Phase, Attempt, Turn), plus Alive (the daemon is running) and
-- Machine (the host whose snapshot this is). rex.exec runs it with argv
-- only; no shell reads the arguments, so issue ids need no quoting.
local function read_snapshot(action_config)
  local r = exec_lambe(lambe_argv({ "logs", "-json" }, action_config, true), 15)
  if not r then
    return nil, "could not start `" .. lambe_bin() .. "`; set LAMBE_REX_BIN or the bin_override variable in lambe.lua"
  end
  if not r.ok then
    local why = (r.stderr ~= "" and r.stderr) or (r.stdout ~= "" and r.stdout) or ("exit " .. tostring(r.code))
    if r.code == 127 then why = "lambe is not on the login shell's PATH (" .. why:gsub("%s+$", "") .. ")" end
    return nil, "lambe logs -json failed: " .. why
  end
  local snapshot, err = rex.json.decode(r.stdout)
  if snapshot == nil then return nil, "lambe logs -json printed no snapshot: " .. tostring(err) end
  return snapshot
end

-- One picker row per issue: current runs first, then history, deduplicated
-- so an issue with an archived earlier attempt still opens its live view.
-- A stopped daemon's last rows are marked, not presented as live. When
-- `workflow` is set only that workflow's runs are listed. Rows stay far
-- below rex.ui.pick's 2,000-item limit because the snapshot itself bounds
-- history.
local function picker_items(snapshot, workflow)
  local items, seen = {}, {}
  local alive = snapshot.Alive ~= false -- older lambe builds omit the field
  local function add(runs, archived)
    for _, run in ipairs(runs or {}) do
      local id = run.IssueID
      local matches = workflow == nil or workflow == "" or run.Workflow == workflow
      if id and id ~= "" and matches and not seen[id] then
        seen[id] = true
        local state = (run.Phase ~= nil and run.Phase ~= "" and run.Phase) or "unknown"
        local parts = {}
        if run.Workflow ~= nil and run.Workflow ~= "" then parts[#parts + 1] = run.Workflow end
        if archived then
          parts[#parts + 1] = "archived"
        elseif not alive and state:sub(1, 8) ~= "stopped:" then
          parts[#parts + 1] = "daemon stopped"
        end
        parts[#parts + 1] = state
        parts[#parts + 1] = "attempt " .. tostring(run.Attempt or 0)
        parts[#parts + 1] = "turn " .. tostring(run.Turn or 0)
        items[#items + 1] = {
          id = id,
          title = (run.Title ~= nil and run.Title ~= "" and run.Title) or id,
          subtitle = table.concat(parts, " · "),
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

-- Distinct workflow names across current and archived runs, with counts.
local function workflow_items(snapshot)
  local counts, order = {}, {}
  local function add(runs)
    for _, run in ipairs(runs or {}) do
      local name = run.Workflow
      if name ~= nil and name ~= "" then
        if counts[name] == nil then
          counts[name] = 0
          order[#order + 1] = name
        end
        counts[name] = counts[name] + 1
      end
    end
  end
  add(snapshot.Runs)
  add(snapshot.History)
  local items = {}
  for _, name in ipairs(order) do
    items[#items + 1] = {
      id = name,
      title = name,
      subtitle = tostring(counts[name]) .. " run" .. (counts[name] == 1 and "" or "s") .. ", current and archived",
      icon = "tag",
    }
  end
  return items
end

local function new_terminal_window(label, command)
  rex.session.new_window{
    window_label = label,
    layout = rex.layout.block{
      flavor = "com.superlogical.terminal.shell",
      options = { command = command, interactive = true },
    },
  }
end

local function open_dashboard(config)
  new_terminal_window("Lambe dashboard", lambe_argv({ "tui" }, config, true))
end

local function open_demo()
  new_terminal_window("Lambe demo", lambe_argv({ "tui", "-demo" }, nil, false))
end

-- Focus the window already following `label` and report whether one
-- existed, so picking the same run twice does not pile up tabs.
local function focus_window_labeled(label)
  local view = rex.session.view{}
  if view == nil then return false end
  for _, window in ipairs(view.windows or {}) do
    if window.label == label and window.window_id ~= nil then
      local focused = rex.session.focus_window{ window_id = window.window_id }
      return focused ~= nil
    end
  end
  return false
end

-- The window label `Lambe · <issue>` is a contract: the LambeDash app
-- focuses an existing window with this label before opening a new one.
local function open_run(issue, config)
  local label = "Lambe · " .. issue
  if focus_window_labeled(label) then return end
  new_terminal_window(label, lambe_argv({ "logs", "-issue", issue, "-follow" }, config, true))
end

local function have_picker()
  return rex.ui and rex.ui.pick and rex.exec and rex.json and rex.json.decode
end

-- Pick one run; `workflow` (optional) narrows the list. Returns the issue
-- id, or nil when nothing was chosen (already logged).
local function choose_run(config, workflow)
  local snapshot, why = read_snapshot(config)
  if snapshot == nil then rex.log("warn", "Lambe: " .. why) return nil end
  local items = picker_items(snapshot, workflow)
  if #items == 0 then
    rex.log("warn", "Lambe: no runs yet; start `lambe run` and queue an issue first")
    return nil
  end
  local where = (snapshot.Machine ~= nil and snapshot.Machine ~= "") and (" on " .. snapshot.Machine) or ""
  local title = "Open Lambe run" .. where .. (workflow ~= nil and workflow ~= "" and (" · " .. workflow) or "")
  local chosen, reason = rex.ui.pick{ title = title, placeholder = "Filter runs", items = items }
  if chosen == nil then
    if reason and reason ~= "cancelled" then rex.log("warn", "Lambe run picker: " .. reason) end
    return nil
  end
  return chosen
end

rex.action{
  name = "lambe_dashboard",
  title = "Lambe: Open Dashboard",
  category = "Lambe",
  description = "Open the Lambe terminal dashboard for this host's daemon",
  keywords = "lambe dashboard tui agents runs queue",
  args = { config = "string?" },
  run = function(_, args) open_dashboard(args.config) end,
}

rex.action{
  name = "lambe_demo",
  title = "Lambe: Open Demo",
  category = "Lambe",
  description = "Explore the Lambe dashboard with sample runs",
  keywords = "lambe demo sample tui",
  run = function() open_demo() end,
}

rex.action{
  name = "lambe_launch",
  title = "Lambe: Choose Dashboard…",
  category = "Lambe",
  keywords = "lambe dashboard demo choose",
  args = { mode = "string?" },
  run = function(_, args)
    local mode = args.mode
    if mode == nil then
      if not (rex.ui and rex.ui.pick) then
        open_dashboard()
        return
      end
      mode = rex.ui.pick{
        title = "Open Lambe",
        items = {
          { id = "live", title = "Your dashboard", subtitle = "Use your default Lambe configuration", icon = "terminal" },
          { id = "demo", title = "Explore the demo", subtitle = "Use sample runs without writing to Beads", icon = "star" },
        },
      }
    end
    if mode == "demo" then open_demo() elseif mode == "live" then open_dashboard() end
  end,
}

rex.action{
  name = "lambe_open_run",
  title = "Lambe: Open Run…",
  category = "Lambe",
  description = "Search current and archived Lambe runs, then follow the chosen one",
  keywords = "lambe run issue logs history archived live output follow",
  args = { issue = "string?", config = "string?", workflow = "string?" },
  run = function(_, args)
    local issue = args.issue
    if issue == nil or issue == "" then
      if not have_picker() then
        open_dashboard(args.config)
        return
      end
      issue = choose_run(args.config, args.workflow)
      if issue == nil then return end
    end
    open_run(issue, args.config)
  end,
}

rex.action{
  name = "lambe_open_workflow_run",
  title = "Lambe: Open Run by Workflow…",
  category = "Lambe",
  description = "Pick a workflow, then one of its current or archived runs",
  keywords = "lambe workflow switch lane route run logs",
  args = { config = "string?" },
  run = function(_, args)
    if not have_picker() then
      open_dashboard(args.config)
      return
    end
    local snapshot, why = read_snapshot(args.config)
    if snapshot == nil then rex.log("warn", "Lambe: " .. why) return end
    local workflows = workflow_items(snapshot)
    local workflow
    if #workflows > 1 then
      local reason
      workflow, reason = rex.ui.pick{ title = "Lambe workflow", placeholder = "Filter workflows", items = workflows }
      if workflow == nil then
        if reason and reason ~= "cancelled" then rex.log("warn", "Lambe workflow picker: " .. reason) end
        return
      end
    end
    local issue = choose_run(args.config, workflow)
    if issue ~= nil then open_run(issue, args.config) end
  end,
}

-- Rex runs these actions on the host the app is connected to, so switching
-- hosts first is how a run opens on the machine that is running its agent.
-- client.host.switch is the native host picker.
rex.action{
  name = "lambe_switch_host",
  title = "Lambe: Switch Host…",
  category = "Lambe",
  description = "Switch Rex to the machine running your agents, then open its Lambe runs",
  keywords = "lambe host machine remote mini switch server",
  run = function() rex.client.queue("client.host.switch") end,
}

-- Runs `lambe doctor` the way every other action runs lambe (login-shell
-- PATH, same config and state directory) and returns its report. `rex do
-- lambe_check` prints it; when an action cannot find lambe, this is the
-- fastest way to see why.
rex.action{
  name = "lambe_check",
  title = "Lambe: Check Setup",
  category = "Lambe",
  description = "Run lambe doctor with the same binary, config, and state the actions use",
  keywords = "lambe doctor check setup troubleshoot path",
  args = { config = "string?" },
  run = function(_, args)
    local r = exec_lambe(lambe_argv({ "doctor" }, args.config, true), 30)
    if not r then return "could not start `" .. lambe_bin() .. "`; set LAMBE_REX_BIN or bin_override" end
    rex.log(r.ok and "info" or "warn", "Lambe doctor exit " .. tostring(r.code))
    return r.stdout .. r.stderr
  end,
}
