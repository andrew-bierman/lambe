-- `lambe logs -json -brief` prints the shared live snapshot as JSON without
-- agent output (the full form is ~400 KB of run tails no picker uses):
-- PascalCase Runs and History rows embedding the progress fields (IssueID,
-- Title, Workflow, Phase, Attempt, Turn) and a per-run Attention
-- {Reason, AgeSec}, plus Alive (the daemon is running), Machine and
-- LambeVersion. A lambe that predates -brief rejects the flag; the read then
-- falls back to plain `-json`, which has no Attention (rows.attention
-- derives it from the phase). rex.exec runs it with argv only; no shell
-- reads the arguments, so issue ids need no quoting.
local host = require("lambe.host")
local exec = require("lambe.exec")
local errors = require("lambe.errors")

local M = {}

-- Set once an installed lambe rejects -brief, so later reads skip the probe.
local brief_unsupported = false

local function failure(r)
  local why = (r.stderr ~= "" and r.stderr) or (r.stdout ~= "" and r.stdout) or ("exit " .. tostring(r.code))
  why = why:gsub("%s+$", "")
  if r.code == 127 then why = "lambe is not on the login shell's PATH: " .. why end
  if r.timed_out then why = "timed out" end
  return why
end

-- Returns the snapshot table; raises with the reason when it cannot.
function M.read(config)
  local sub = { "logs", "-json", "-brief" }
  if brief_unsupported then sub = { "logs", "-json" } end
  local r = exec.run(exec.command(sub, config, true), 15)
  if not r.ok and #sub == 3 and not r.timed_out and r.code ~= 127 and failure(r):find("brief", 1, true) then
    brief_unsupported = true
    sub = { "logs", "-json" }
    r = exec.run(exec.command(sub, config, true), 15)
  end
  local name = "`lambe " .. table.concat(sub, " ") .. "`"
  if not r.ok then errors.fail(name .. " failed", failure(r)) end
  local snapshot, err = host.json_decode(r.stdout)
  if type(snapshot) ~= "table" then errors.fail(name .. " printed no snapshot", err) end
  return snapshot
end

return M
