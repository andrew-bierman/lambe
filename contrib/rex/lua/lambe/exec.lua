-- Building and running `lambe` invocations. Every live invocation passes
-- the same -config, binary, and state directory, so they all read the same
-- daemon state. Demo mode uses sample data.
local host = require("lambe.host")
local errors = require("lambe.errors")

local M = {}

-- Settings from lambe.config.lua: bin, config, state_dir.
local settings = {}

function M.configure(cfg) settings = cfg or {} end

function M.setting(name) return settings[name] end

-- Returns the first non-empty string argument. select("#") counts nil
-- arguments, which ipairs would stop at.
local function first_set(...)
  for i = 1, select("#", ...) do
    local value = select(i, ...)
    if type(value) == "string" and value ~= "" then return value end
  end
  return ""
end

-- Precedence: explicit action argument (config only), then the Rex
-- server's environment, then lambe.config.lua, then Lambe's defaults.
function M.bin() return first_set(host.getenv("LAMBE_REX_BIN"), settings.bin, "lambe") end

-- A spec is {argv, env}: the program and arguments, and variables to set.
function M.command(sub, config, live)
  local argv = { M.bin() }
  for _, arg in ipairs(sub) do argv[#argv + 1] = arg end
  local env = {}
  if live then
    local cfg = first_set(config, host.getenv("LAMBE_REX_CONFIG"), settings.config)
    if cfg ~= "" then
      argv[#argv + 1] = "-config"
      argv[#argv + 1] = cfg
    end
    -- The daemon's state directory decides which snapshot `lambe logs`
    -- reads; a daemon started under launchd or systemd must be read with
    -- the same value.
    local state_dir = first_set(host.getenv("LAMBE_REX_STATE_DIR"), settings.state_dir)
    if state_dir ~= "" then env.LAMBE_STATE_DIR = state_dir end
  end
  return { argv = argv, env = env }
end

-- Terminal block commands have no env option, so the variables go through
-- env(1) there.
function M.terminal_argv(spec)
  local out = {}
  if next(spec.env) ~= nil then
    out[1] = "env"
    for name, value in pairs(spec.env) do out[#out + 1] = name .. "=" .. value end
  end
  for _, arg in ipairs(spec.argv) do out[#out + 1] = arg end
  return out
end

-- rex.exec starts programs with a minimal PATH (/usr/bin:/bin:...), so a
-- bare `lambe` in ~/.local/bin is invisible to it. Bare names run through a
-- login shell; `lambe setup rex` bakes an absolute path, which runs
-- directly.
local posix_shells = { sh = true, bash = true, zsh = true, dash = true, ksh = true }

local function login_shell()
  local shell = host.getenv("SHELL") or ""
  if posix_shells[shell:match("([^/]+)$") or ""] then return shell end
  -- A launchd-started server has no $SHELL, and macOS's /bin/sh -l does not
  -- read ~/.zprofile, where PATH usually lives.
  if (host.getenv("HOME") or ""):sub(1, 7) == "/Users/" then return "/bin/zsh" end
  return "/bin/sh"
end

-- Run a spec and return rex.exec's result table; raise when it cannot start.
function M.run(spec, timeout)
  local argv = spec.argv
  if not argv[1]:find("/", 1, true) then
    local wrapped = { login_shell(), "-l", "-c", 'exec "$@"', "lambe" }
    for _, arg in ipairs(argv) do wrapped[#wrapped + 1] = arg end
    argv = wrapped
  end
  local result, err = host.exec(argv, { timeout = timeout, env = next(spec.env) ~= nil and spec.env or nil })
  if result == nil then
    errors.fail("could not start `" .. M.bin() .. "`; set bin in lambe.config.lua or LAMBE_REX_BIN", err)
  end
  return result
end

return M
