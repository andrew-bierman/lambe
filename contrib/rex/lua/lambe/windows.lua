-- Rex windows for Lambe output. Tab titles: the window label
-- `Lambe · <issue>` is the lookup key shared with the LambeDash app, so it
-- never changes; the pane label carries the readable title
-- `ISSUE · Workflow · state`. last_titles remembers what this script last
-- set per block, so a pane the user renamed is left alone. It lives as long
-- as the Rex server's config; after a reload nothing is known to be ours,
-- so nothing is renamed (it errs toward never overwriting).
local host = require("lambe.host")

local M = {}

local last_titles = {}

function M.new_terminal_window(label, command, title)
  local layout = host.block{
    flavor = "com.superlogical.terminal.shell",
    label = title,
    options = { command = command, interactive = true },
  }
  local created = host.new_window{ window_label = label, layout = layout }
  local block = created and created.block_ids and created.block_ids[1]
  if block ~= nil and title ~= nil then last_titles[block] = title end
end

-- The window already following `label`, as {window_id, block_id,
-- block_label}, or nil, so picking the same run twice does not pile up tabs.
function M.find_labeled(label)
  local view = host.view()
  if view == nil then return nil end
  for _, window in ipairs(view.windows or {}) do
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
  return nil
end

-- Refresh the title of a reused tab, unless the user changed it.
function M.retitle(found, title)
  if found.block_id == nil or last_titles[found.block_id] ~= found.block_label then return end
  if title == found.block_label then return end
  host.set_block_label(found.block_id, title)
  last_titles[found.block_id] = title
end

-- Focus the window if it exists; returns whether it did.
function M.focus_existing(label, title)
  local found = M.find_labeled(label)
  if found ~= nil and host.focus_window(found.window_id) ~= nil then
    M.retitle(found, title)
    return true
  end
  return false
end

return M
