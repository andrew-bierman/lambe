-- Rex windows and panes for Lambe output. The window label
-- `Lambe · <issue>` is the lookup key shared with the LambeDash app, so it
-- never changes; the pane label carries the readable title
-- `ISSUE · Workflow · state`. last_titles remembers what this script last
-- set per block, so a pane the user renamed is left alone. It lives as long
-- as the Rex server's config; after a reload nothing is known to be ours,
-- so nothing is renamed (it errs toward never overwriting).
local host = require("lambe.host")

local M = {}

local last_titles = {}

-- With `keep_open` the terminal outlives its command, so a finished run's
-- output stays readable until the user closes the tab. The dashboard closes
-- when the user quits it.
local function terminal_block(command, title, keep_open)
  return host.block{
    flavor = "com.superlogical.terminal.shell",
    label = title,
    options = { command = command, interactive = true, exit = keep_open and { on_completion = false } or nil },
  }
end

function M.new_terminal_window(label, command, title, keep_open)
  local created = host.new_window{ window_label = label, layout = terminal_block(command, title, keep_open) }
  local block = created and created.block_ids and created.block_ids[1]
  if block ~= nil and title ~= nil then last_titles[block] = title end
end

-- A split beside `anchor_id` (the block the action ran from; the active
-- window's focused block when nil).
function M.new_split(anchor_id, command, title)
  local created = host.new_split{
    anchor_id = anchor_id,
    direction = "horizontal",
    side = "after",
    ratio = 0.5,
    layout = terminal_block(command, title, true),
  }
  local block = created and created.block_ids and created.block_ids[1]
  if block ~= nil and title ~= nil then last_titles[block] = title end
end

-- The first block of a window view's window labeled `label`.
local function find_in_view(view, label)
  for _, window in ipairs(view and view.windows or {}) do
    if window.label == label and window.window_id ~= nil then
      local found = { window_id = window.window_id }
      for _, layer in ipairs(window.layers or {}) do
        for _, block in ipairs(layer.blocks or {}) do
          if found.block_id == nil then
            found.block_id, found.block_label = block.block_id, block.label
          end
        end
      end
      return found
    end
  end
end

-- The window already following `label` in this session:
-- {window_id, block_id, block_label}, or nil.
function M.find_labeled(label)
  return find_in_view(host.view(), label)
end

-- Look for `label` in the other sessions on the server and focus it where it
-- lives. Returns that session's label, or nil when none has it.
local function focus_elsewhere(label, own_session)
  for _, session in ipairs(host.sessions()) do
    if session.session_id ~= own_session then
      local focused = host.with_session(session.session_id, function(view)
        local found = find_in_view(view, label)
        return found ~= nil and host.focus_window_in(session.session_id, found.window_id) ~= nil
      end)
      if focused then return session.label end
    end
  end
end

-- The placed pane in this session whose title starts with `ISSUE · ` (or is
-- the bare id): the split-placement counterpart of the window label.
function M.find_pane(issue)
  local listed = host.list_blocks()
  local prefix = issue .. " · "
  for _, block in ipairs(listed and listed.blocks or {}) do
    local label = block.label or ""
    if block.placement == "placed" and block.block_id ~= nil
      and (label == issue or label:sub(1, #prefix) == prefix) then
      return { block_id = block.block_id, block_label = label }
    end
  end
end

-- Refresh the title of a reused tab, unless the user changed it.
function M.retitle(found, title)
  if found.block_id == nil or last_titles[found.block_id] ~= found.block_label then return end
  if title == found.block_label then return end
  host.set_block_label(found.block_id, title)
  last_titles[found.block_id] = title
end

-- Focus the window if it exists. Returns whether one existed, and a message
-- when it lives in another session (focusing it there changes nothing the
-- user is looking at, so say where it is).
function M.focus_existing(label, title, own_session)
  local found = M.find_labeled(label)
  if found ~= nil then
    if host.focus_window(found.window_id) == nil then return false end
    M.retitle(found, title)
    return true
  end
  local where = focus_elsewhere(label, own_session)
  if where == nil then return false end
  return true, "`" .. label .. "` is already open in session `" .. tostring(where) .. "`; switch to that session to see it"
end

-- Focus the pane for `issue` if one exists; returns whether it did.
function M.focus_pane(issue, title)
  local found = M.find_pane(issue)
  if found == nil or host.focus_block(found.block_id) == nil then return false end
  M.retitle(found, title)
  return true
end

return M
