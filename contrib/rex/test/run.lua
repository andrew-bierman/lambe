-- Pure-Lua test runner for the Lambe Rex integration. Needs only luajit
-- (Lua 5.1, the dialect Rex runs):
--
--   luajit contrib/rex/test/run.lua            run every spec
--   UPDATE_GOLDEN=1 luajit contrib/rex/test/run.lua   rewrite golden files
local here = (arg[0]:match("^(.*)/[^/]*$") or ".")
local root = here .. "/../lua"
package.path = here .. "/?.lua;" .. root .. "/?.lua;" .. package.path

local T = { passed = 0, failed = {}, here = here, root = root }

function T.eq(got, want, what)
  if got ~= want then
    error(string.format("%s:\n  got  %s\n  want %s", what or "values differ", tostring(got), tostring(want)), 2)
  end
end

function T.ok(cond, what) if not cond then error(what or "assertion failed", 2) end end

function T.contains(haystack, needle, what)
  if type(haystack) ~= "string" or not haystack:find(needle, 1, true) then
    error(string.format("%s: %q not found in %q", what or "missing text", needle, tostring(haystack)), 2)
  end
end

-- fresh_modules drops every lambe.* module so each test loads clean state.
function T.fresh_modules()
  for name in pairs(package.loaded) do
    if name:sub(1, 6) == "lambe." then package.loaded[name] = nil end
  end
end

-- load_shim runs lua/lambe.lua the way an installed copy runs, with its
-- directory placeholder replaced; `dir` defaults to the source tree and may
-- hold a lambe.config.lua.
function T.load_shim(dir)
  local f = assert(io.open(root .. "/lambe.lua", "r"))
  local source = f:read("*a")
  f:close()
  source = source:gsub('"@LAMBE_DIR@"', function() return string.format("%q", dir or root) end)
  return assert(loadstring(source, "@lambe.lua"))()
end

function T.test(name, fn)
  T.fresh_modules()
  local ok, err = xpcall(fn, function(e) return tostring(e) end)
  if ok then
    T.passed = T.passed + 1
  else
    T.failed[#T.failed + 1] = name .. "\n    " .. err:gsub("\n", "\n    ")
  end
end

for _, spec in ipairs({ "spec_rows", "spec_errors", "spec_actions", "spec_stub" }) do
  dofile(here .. "/" .. spec .. ".lua")(T)
end

print(string.format("%d passed, %d failed", T.passed, #T.failed))
for _, f in ipairs(T.failed) do print("FAIL " .. f) end
os.exit(#T.failed == 0 and 0 or 1)
