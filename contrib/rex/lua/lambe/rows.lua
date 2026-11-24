-- Pure row building: snapshot tables in, picker rows and titles out. No
-- rex.* calls, so every function is testable on its own.
local M = {}

-- rex.ui.pick accepts at most 2000 items.
M.MAX_ITEMS = 2000

-- One picker row per issue: current runs first, then history, deduplicated
-- so an issue with an archived earlier attempt still opens its live view.
-- A stopped daemon's last rows are marked, not presented as live. When
-- `workflow` is set only that workflow's runs are listed. Rows stay far
-- below rex.ui.pick's 2,000-item limit because the snapshot itself bounds
-- history.
function M.picker_items(snapshot, workflow)
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
        -- Status marker first: ● live, ◌ last known state of a stopped
        -- daemon, ○ archived. Live runs are listed before archived ones.
        local stale = not archived and not alive and state:sub(1, 8) ~= "stopped:"
        local marker = archived and "○" or (stale and "◌" or "●")
        if run.Workflow ~= nil and run.Workflow ~= "" then parts[#parts + 1] = run.Workflow end
        if archived then
          parts[#parts + 1] = "archived"
        elseif stale then
          parts[#parts + 1] = "daemon stopped"
        end
        parts[#parts + 1] = state
        parts[#parts + 1] = "attempt " .. tostring(run.Attempt or 0)
        parts[#parts + 1] = "turn " .. tostring(run.Turn or 0)
        items[#items + 1] = {
          id = id,
          title = (run.Title ~= nil and run.Title ~= "" and run.Title) or id,
          subtitle = marker .. " " .. table.concat(parts, " · "),
          detail = id,
          icon = archived and "clock" or "terminal",
        }
      end
    end
  end
  add(snapshot.Runs, false)
  add(snapshot.History, true)
  while #items > M.MAX_ITEMS do items[#items] = nil end
  return items
end


-- Distinct workflow names across current and archived runs, with counts.
function M.workflow_items(snapshot)
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


-- Pane title `ISSUE · Workflow · state` for the run with this id.
function M.run_title(snapshot, issue)
  local found
  for _, list in ipairs({ snapshot and snapshot.Runs or {}, snapshot and snapshot.History or {} }) do
    for _, run in ipairs(list) do
      if run.IssueID == issue and found == nil then found = run end
    end
  end
  local parts = { issue }
  if found ~= nil then
    if found.Workflow ~= nil and found.Workflow ~= "" then parts[#parts + 1] = found.Workflow end
    if found.Phase ~= nil and found.Phase ~= "" then parts[#parts + 1] = found.Phase end
  end
  return table.concat(parts, " · ")
end

return M
