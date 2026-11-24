-- Error policy. `error(msg, 0)` inside a Rex action reaches whoever ran it
-- (palette, `rex do`, CLI); a log line alone is invisible to a palette user.
local M = {}

-- Raise a user-facing failure; `why` (a Rex reason or detail) is appended.
function M.fail(msg, why)
  if why ~= nil and why ~= "" then msg = msg .. " (" .. tostring(why) .. ")" end
  error("Lambe: " .. msg, 0)
end

-- Interpret rex.ui.pick's results: the choice, nil when the user
-- cancelled, or a raised failure for every other reason.
function M.picked(chosen, reason, detail, what)
  if chosen ~= nil then return chosen end
  if reason == nil or reason == "cancelled" then return nil end
  local why = reason
  if detail ~= nil and detail ~= "" then why = why .. ": " .. tostring(detail) end
  return M.fail(what .. " failed", why)
end

return M
