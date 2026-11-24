-- Snapshot fixtures in the shape `lambe logs -json` prints.
local M = {}

local function run(id, title, workflow, phase, attempt, turn)
  return { IssueID = id, Title = title, Workflow = workflow, Phase = phase, Attempt = attempt, Turn = turn }
end

M.alive = {
  Alive = true, Machine = "mini",
  Runs = { run("ac-1", "Fix the login redirect", "build", "running", 1, 3), run("ac-2", "", "build", "gating", 2, 4) },
  History = { run("ac-3", "Old cleanup", "review", "stopped: gate escalation", 3, 9), run("ac-1", "Fix the login redirect", "build", "stopped: done", 1, 7) },
}

M.stopped_daemon = {
  Alive = false, Machine = "laptop",
  Runs = { run("ac-9", "Wedged run", "build", "running", 0, 0), run("ac-8", "Finished", "build", "stopped: done", 1, 2) },
  History = {},
}

M.legacy_no_alive = { Runs = { run("x-1", "Legacy build", "", "legacy-blocked", 0, 0) }, History = {} }

M.unicode = {
  Alive = true,
  Runs = { run("ac-7", "Résumé ünïcode · 日本語 titles", "plan", "running", 1, 1) },
  History = {},
}

M.empty = { Alive = true, Machine = "mini", Runs = {}, History = {} }

M.two_workflows = {
  Alive = true,
  Runs = { run("a-1", "A", "build", "running", 1, 1), run("a-2", "B", "review", "running", 1, 1), run("a-3", "C", "build", "running", 1, 1) },
  History = { run("a-4", "D", "review", "stopped: done", 1, 1) },
}

function M.many(n)
  local history = {}
  for i = 1, n do history[i] = run("h-" .. i, "History " .. i, "build", "stopped: done", 1, 1) end
  return { Alive = true, Runs = {}, History = history }
end

return M
