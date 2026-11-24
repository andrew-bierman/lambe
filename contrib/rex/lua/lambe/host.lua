-- The only module that touches the global `rex` (and the process
-- environment). Everything else goes through here so a test stub covers
-- the whole surface.
local rex = rex
local lambe_version = require("lambe.version")

local M = {}

function M.getenv(name) return os.getenv(name) end
function M.log(level, message) rex.log(level, message) end
function M.action(def) return rex.action(def) end
function M.exec(argv, opts) return rex.exec(argv, opts) end
function M.json_decode(text) return rex.json.decode(text) end
function M.block(spec) return rex.layout.block(spec) end
function M.new_window(args) return rex.session.new_window(args) end
function M.view() return rex.session.view{} end
function M.focus_window(window_id) return rex.session.focus_window{ window_id = window_id } end
function M.set_block_label(block_id, label) return rex.session.set_block_label{ block_id = block_id, label = label } end
function M.new_split(args) return rex.session.new_split(args) end
function M.list_blocks() return rex.session.list_blocks{} end
function M.focus_block(block_id) return rex.session.focus_block{ block_id = block_id } end
function M.queue_client(name) return rex.client.queue(name) end

-- Whether this build of Rex offers the picker. rex.ui exists from API
-- revision 17 (preview.21), but only the revision is authoritative when the
-- server can say it. Returns ok, and the reason it is missing.
local api_revision
function M.api_revision()
  if api_revision ~= nil then return api_revision end
  if rex.call == nil then return nil end
  local ok, result = pcall(rex.call, "server.version")
  if ok and type(result) == "table" and type(result.api_revision) == "number" then
    api_revision = result.api_revision
  end
  return api_revision
end

function M.picker_support()
  local revision = M.api_revision()
  local present = rex.ui ~= nil and rex.ui.pick ~= nil
  if present and (revision == nil or revision >= lambe_version.UI_API) then return true end
  local seen = revision and ("this server reports API revision " .. revision) or "this server has no rex.ui"
  return false, "the run picker needs Rex preview.21 or newer (API revision " .. lambe_version.UI_API .. "+); " .. seen
end

-- Other sessions on this server: {session_id, label} rows, their window
-- views, and focusing a window there. rex.call works after config load.
function M.sessions()
  local result = rex.call("session.list")
  return result and result.sessions or {}
end

function M.session_view(session_id) return rex.call("session.view", { session_id = session_id }) end

function M.focus_window_in(session_id, window_id)
  return rex.call("session.focus_window", { session_id = session_id, window_id = window_id })
end

function M.pick(opts) return rex.ui.pick(opts) end

return M
