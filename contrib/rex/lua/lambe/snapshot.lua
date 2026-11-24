-- `lambe logs -json` prints the shared live snapshot as JSON: PascalCase
-- Runs and History rows embedding the progress fields (IssueID, Title,
-- Workflow, Phase, Attempt, Turn), plus Alive (the daemon is running) and
-- Machine (the host whose snapshot this is). rex.exec runs it with argv
-- only; no shell reads the arguments, so issue ids need no quoting.
local host = require("lambe.host")
local exec = require("lambe.exec")
local errors = require("lambe.errors")

local M = {}

-- Returns the snapshot table; raises with the reason when it cannot.
function M.read(config)
  local r = exec.run(exec.command({ "logs", "-json" }, config, true), 15)
  if not r.ok then
    local why = (r.stderr ~= "" and r.stderr) or (r.stdout ~= "" and r.stdout) or ("exit " .. tostring(r.code))
    why = why:gsub("%s+$", "")
    if r.code == 127 then why = "lambe is not on the login shell's PATH: " .. why end
    if r.timed_out then why = "timed out" end
    errors.fail("`lambe logs -json` failed", why)
  end
  local snapshot, err = host.json_decode(r.stdout)
  if type(snapshot) ~= "table" then errors.fail("`lambe logs -json` printed no snapshot", err) end
  return snapshot
end

return M
