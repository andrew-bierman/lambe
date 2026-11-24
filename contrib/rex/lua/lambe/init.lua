-- Entry point: `require("lambe.init").setup(config)` registers the actions.
local version = require("lambe.version")

local M = { VERSION = version.VERSION }

-- `cfg` comes from lambe.config.lua: bin, config, state_dir (all strings).
function M.setup(cfg)
  cfg = cfg or {}
  require("lambe.exec").configure(cfg)
  require("lambe.actions").register()
  return M
end

return M
