-- Pure row building: snapshot tables in, picker rows and titles out. No
-- rex.* calls, so every function is testable on its own.
local M = {}

-- rex.ui.pick accepts at most 2000 items.
M.MAX_ITEMS = 2000

-- Phases that mean a human has to act. This mirrors the dashboard's
-- needsAttention and is only the fallback: a run carrying `Attention`
-- (`{ Reason = "...", AgeSec = n }`, supplied by newer lambe builds) wins.
local NEEDS_YOU = {
  retry = "retrying",
  deferred = "waiting to retry",
  interrupted = "interrupted",
  ["stopped: gate escalation"] = "gate escalation",
  ["stopped: gate red"] = "gate failed",
}

-- Plain-language phase names; anything unknown is shown as is.
local HUMAN = {
  gating = "checking gates",
  ["legacy-blocked"] = "blocked (legacy)",
  ["stopped: done"] = "finished",
  retry = "retrying",
  deferred = "waiting to retry",
  interrupted = "interrupted",
  ["stopped: gate escalation"] = "gate escalation",
  ["stopped: gate red"] = "gate failed",
}

function M.humanize(phase)
  if phase == nil or phase == "" then return "unknown" end
  return HUMAN[phase] or phase
end

-- The reason a run needs a human, or nil when it does not.
function M.attention(run)
  if type(run.Attention) == "table" and run.Attention.Reason ~= nil and run.Attention.Reason ~= "" then
    return run.Attention.Reason
  end
  return NEEDS_YOU[run.Phase or ""]
end

-- "45s", "14m", "3h", "2d" for an age in seconds; nil when unknown.
function M.age(seconds)
  if type(seconds) ~= "number" or seconds < 0 then return nil end
  if seconds < 60 then return string.format("%ds", seconds) end
  if seconds < 3600 then return string.format("%dm", seconds / 60) end
  if seconds < 86400 then return string.format("%dh", seconds / 3600) end
  return string.format("%dd", seconds / 86400)
end

local function present(value) return value ~= nil and value ~= "" end

-- One row per issue, in the order needs-you, live, archived. A run that is
-- both in Runs and History (a retried issue) shows once, from its freshest
-- record. A stopped daemon's last rows are marked, not presented as live.
-- `workflow` narrows the list; `only_attention` keeps just the runs that
-- need a human. The snapshot bounds history, so rows stay under
-- MAX_ITEMS in practice and are capped regardless.
function M.picker_items(snapshot, workflow, only_attention)
  local alive = snapshot.Alive ~= false -- older lambe builds omit the field
  local groups = { needs = {}, live = {}, archived = {} }
  local seen = {}
  local function add(runs, archived)
    for _, run in ipairs(runs or {}) do
      local id = run.IssueID
      local matches = not present(workflow) or run.Workflow == workflow
      if present(id) and matches and not seen[id] then
        seen[id] = true
        local reason = M.attention(run)
        local stale = not archived and not alive and (run.Phase or ""):sub(1, 8) ~= "stopped:"
        local marker, group, icon
        if reason then
          marker, group, icon = "▲", "needs", "star"
        elseif archived then
          marker, group, icon = "○", "archived", "clock"
        elseif stale then
          marker, group, icon = "◌", "live", "terminal"
        else
          marker, group, icon = "●", "live", "terminal"
        end
        local parts = {}
        if present(run.Workflow) then parts[#parts + 1] = run.Workflow end
        if archived and not reason then
          parts[#parts + 1] = "archived"
        elseif stale and not reason then
          parts[#parts + 1] = "daemon stopped"
        end
        parts[#parts + 1] = reason and ("needs you: " .. reason) or M.humanize(run.Phase)
        if (run.Attempt or 0) > 1 then parts[#parts + 1] = "attempt " .. tostring(run.Attempt) end
        if (run.Turn or 0) > 0 then parts[#parts + 1] = "turn " .. tostring(run.Turn) end
        local age = M.age((type(run.Attention) == "table" and run.Attention.AgeSec) or run.AgeSec)
        if age then parts[#parts + 1] = age end
        if not (only_attention and not reason) then
          local title = present(run.Title) and run.Title or id
          table.insert(groups[group], {
            id = id,
            title = title,
            subtitle = marker .. " " .. table.concat(parts, " · "),
            detail = title ~= id and id or nil,
            icon = icon,
          })
        end
      end
    end
  end
  add(snapshot.Runs, false)
  add(snapshot.History, true)
  local items = {}
  for _, name in ipairs({ "needs", "live", "archived" }) do
    for _, item in ipairs(groups[name]) do
      if #items < M.MAX_ITEMS then items[#items + 1] = item end
    end
  end
  return items
end

-- How many runs need a human, across current and archived runs.
function M.attention_count(snapshot)
  return #M.picker_items(snapshot, nil, true)
end

-- Distinct workflow names across current and archived runs, each with the
-- number of distinct issues, so the count matches the run list it opens.
function M.workflow_items(snapshot)
  local ids, order = {}, {}
  local function add(runs)
    for _, run in ipairs(runs or {}) do
      local name = run.Workflow
      if present(name) and present(run.IssueID) then
        if ids[name] == nil then
          ids[name] = { count = 0, seen = {} }
          order[#order + 1] = name
        end
        local entry = ids[name]
        if not entry.seen[run.IssueID] then
          entry.seen[run.IssueID] = true
          entry.count = entry.count + 1
        end
      end
    end
  end
  add(snapshot.Runs)
  add(snapshot.History)
  local items = {}
  for _, name in ipairs(order) do
    local n = ids[name].count
    items[#items + 1] = {
      id = name,
      title = name,
      subtitle = tostring(n) .. " run" .. (n == 1 and "" or "s") .. ", current and archived",
      icon = "tag",
    }
  end
  return items
end

-- What to tell the user when there is nothing to pick from.
function M.empty_message(snapshot, workflow, only_attention)
  if only_attention then return "Nothing needs you right now." end
  if present(workflow) then return "no runs for workflow `" .. workflow .. "` yet" end
  if snapshot.Alive == false then
    return "no runs recorded and the daemon is stopped; start it with `lambe run` or `lambe service install`"
  end
  return "no runs yet; queue an issue and the daemon will pick it up"
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
    if present(found.Workflow) then parts[#parts + 1] = found.Workflow end
    if present(found.Phase) then parts[#parts + 1] = found.Phase end
  end
  return table.concat(parts, " · ")
end

return M
