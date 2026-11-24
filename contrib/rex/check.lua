-- Headless check of the lambe.lua shim and its modules against a real `lambe logs -json` snapshot.
--
--   LAMBE_REX_LUA=~/.config/rex/lambe.lua rex do contrib/rex/check.lua
--
-- It loads lambe.lua with a stubbed `rex` that records what the actions
-- would open and answers every picker with its first choice, so it needs no
-- Rex app and moves no one's focus. Run it with the same LAMBE_REX_*
-- environment (or `override` variables) your actions use. It prints PASS and
-- the rows the picker would show, or raises on the first failed assertion.
-- rex.exec here inherits the CLI's PATH, not the server's; use
-- `rex do lambe_check` to test the server's own environment.

local path = os.getenv("LAMBE_REX_LUA") or (os.getenv("HOME") .. "/.config/rex/lambe.lua")
local dir = path:match("^(.*)/[^/]*$") or "."
local real = rex
local actions, opened, focused, queued, picks = {}, {}, {}, {}, {}
local labeled = {} -- window labels the stubbed session already shows
local block_labels = {} -- window label -> {id, label} of its pane
local renamed = {}

local stub = setmetatable({
  action = function(def) actions[def.name] = def end,
  ui = { pick = function(opts)
    picks[#picks + 1] = opts
    return opts.items[1] and opts.items[1].id or nil
  end },
  session = {
    view = function() 
      local windows = {}
      for label, id in pairs(labeled) do
        local b = block_labels[label] or { id = "block:" .. id, label = "" }
        windows[#windows + 1] = { label = label, window_id = id,
          layers = { { blocks = { { block_id = b.id, label = b.label } } } } }
      end
      return { windows = windows }
    end,
    focus_window = function(a) focused[#focused + 1] = a.window_id return {} end,
    new_window = function(a)
      opened[#opened + 1] = a
      return { window_id = "window:" .. #opened, block_ids = { "block:" .. #opened } }
    end,
    set_block_label = function(a) renamed[#renamed + 1] = a return {} end,
  },
  log = function(level, message) print(level .. ": " .. tostring(message)) end,
  layout = { block = function(a) return a end }, -- keep the options readable
  client = { queue = function(name) queued[#queued + 1] = name end },
  call = function() return { api_revision = 18 } end, -- pretend the server has rex.ui
}, { __index = real })

rex = stub
-- Load the shim the way `lambe setup rex` installs it: its directory placeholder
-- filled in, the modules and lambe.config.lua beside it. The actions keep using
-- the stub until the script ends.
local handle = assert(io.open(path, "r"))
local source = handle:read("*a"):gsub('"@LAMBE_DIR@"', function() return string.format("%q", dir) end)
handle:close()
assert(loadstring(source, "@" .. path))()

local function check(ok, message) if not ok then error("check failed: " .. message, 0) end end
local function command(window) return table.concat(window.layout.options.command, " ") end

for _, name in ipairs({ "lambe_dashboard", "lambe_demo", "lambe_launch", "lambe_open_run", "lambe_open_workflow_run", "lambe_switch_host", "lambe_check" }) do
  check(actions[name], "missing action " .. name)
end

actions.lambe_open_run.run({}, {})
check(#picks == 1, "Open Run… showed no picker (no runs, or lambe failed; see the log)")
local rows = picks[1].items
check(#opened == 1, "choosing a run opened no window")
local label = opened[1].window_label
check(label == "Lambe · " .. rows[1].id, "window label is not `Lambe · <issue>`: " .. tostring(label))
check(command(opened[1]):find("logs -issue " .. rows[1].id .. " -follow", 1, true), "window does not follow the chosen run")

local title = opened[1].layout.label
check(title and title:sub(1, #rows[1].id + 4) == rows[1].id .. " · ", "pane title is not `ISSUE · Workflow · state`: " .. tostring(title))

labeled[label] = "window:existing"
block_labels[label] = { id = "block:1", label = title } -- still the title Lambe set
actions.lambe_open_run.run({}, {})
check(#opened == 1 and focused[1] == "window:existing", "an open run window was not reused")

block_labels[label] = { id = "block:1", label = "my own name" } -- the user renamed it
local before = #renamed
actions.lambe_open_run.run({}, {})
check(#renamed == before, "a pane the user renamed was overwritten")

actions.lambe_open_run.run({}, { issue = "x-1" })
check(opened[2].window_label == "Lambe · x-1", "issue argument skipped the picker incorrectly")
check(#picks >= 2, "issue argument must not open the picker")

actions.lambe_open_workflow_run.run({}, {})
actions.lambe_switch_host.run({}, {})
check(queued[1] == "client.host.switch", "Switch Host… did not ask the app for its host picker")

print("PASS " .. path)
for _, row in ipairs(rows) do print(string.format("  %-10s %-34s %s", row.id, row.title, row.subtitle)) end
print("  picker title: " .. picks[1].title)
