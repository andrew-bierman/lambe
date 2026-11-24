-- A strict stand-in for Rex's Lua API (Lua 5.1, run by luajit). It enforces
-- the documented shapes, so a test that passes here would not have been
-- rejected by a real server, and APIs a server lacks are really nil.
--
--   local stub = require("rexstub").install("p21")   -- sets the global `rex`
--   stub.answers = { "ISSUE-1" }                      -- scripted picker answers
--   stub.exec_handler = function(argv, opts) return { code = 0, stdout = "" } end
--
-- Profiles: p16 (API 13, no rex.ui), p19 (API 14, no rex.ui), p21 (API 18).
local M = {}

local PROFILES = {
  p16 = { api_revision = 13, ui = false },
  p19 = { api_revision = 14, ui = false },
  p21 = { api_revision = 18, ui = true },
}

local ICONS = {}
for _, name in ipairs({ "document", "folder", "branch", "terminal", "code", "server", "database", "package",
  "globe", "link", "person", "tag", "clock", "star", "gear", "plus" }) do ICONS[name] = true end

local function fail(msg) error("rexstub: " .. msg, 3) end

-- Request schemas captured from Rex itself (contrib/rex/testdata/api,
-- rendered by contrib/rex/internal/gen).
local SCHEMA = require("apischema")

local LUA_TYPE = { string = "string", number = "number", integer = "number", boolean = "boolean", object = "table", array = "table" }

-- validate checks request args for `method` against the schema of the
-- profile's API revision: the newest captured revision not above it, else
-- the oldest captured one. `injected` is true for rex.session.*, where the
-- Lua wrapper supplies session_id.
local function validate(api_revision, method, args, injected)
  local best, oldest
  for rev, methods in pairs(SCHEMA) do
    if methods[method] then
      if rev <= api_revision and (best == nil or rev > best) then best = rev end
      if oldest == nil or rev < oldest then oldest = rev end
    end
  end
  local spec = SCHEMA[best or oldest] and SCHEMA[best or oldest][method]
  if spec == nil then return end -- no captured schema for this method
  args = args or {}
  if type(args) ~= "table" then fail(method .. " takes a table") end
  for name, value in pairs(args) do
    local field = spec.fields[name]
    if field == nil then
      if spec.closed then fail(method .. ": unknown field `" .. tostring(name) .. "`") end
    else
      local ok = false
      for _, t in ipairs(field.types) do
        if t ~= "null" and LUA_TYPE[t] == type(value) then ok = true end
      end
      if not ok then fail(method .. ": field `" .. name .. "` must be " .. table.concat(field.types, "|")) end
      if field.enum then
        local found = false
        for _, e in ipairs(field.enum) do if e == value then found = true end end
        if not found then fail(method .. ": field `" .. name .. "` must be one of " .. table.concat(field.enum, ", ")) end
      end
    end
  end
  for _, name in ipairs(spec.required) do
    if args[name] == nil and not (injected and name == "session_id") then
      fail(method .. ": missing required field `" .. name .. "`")
    end
  end
end

local function check_keys(what, tbl, allowed)
  for key in pairs(tbl) do
    if not allowed[key] then fail(what .. ": unknown field `" .. tostring(key) .. "`") end
  end
end

local function set(list)
  local out = {}
  for _, v in ipairs(list) do out[v] = true end
  return out
end

-- Minimal JSON decoder (objects, arrays, strings, numbers, true/false/null).
local function json_decode(text)
  local pos = 1
  local function skip() pos = text:find("%S", pos) or #text + 1 end
  local value
  local function str()
    local out, i = {}, pos + 1
    while true do
      local c = text:sub(i, i)
      if c == "" then error("unterminated string") end
      if c == '"' then pos = i + 1 return table.concat(out) end
      if c == "\\" then
        local n = text:sub(i + 1, i + 1)
        local map = { n = "\n", t = "\t", r = "\r", b = "\b", f = "\f", ["/"] = "/", ["\\"] = "\\", ['"'] = '"' }
        if n == "u" then
          local cp = tonumber(text:sub(i + 2, i + 5), 16)
          out[#out + 1] = cp < 128 and string.char(cp) or "?"
          i = i + 6
        else
          out[#out + 1] = map[n] or n
          i = i + 2
        end
      else
        out[#out + 1] = c
        i = i + 1
      end
    end
  end
  function value()
    skip()
    local c = text:sub(pos, pos)
    if c == "{" then
      local obj = {}
      pos = pos + 1
      skip()
      if text:sub(pos, pos) == "}" then pos = pos + 1 return obj end
      while true do
        skip()
        local key = str()
        skip()
        assert(text:sub(pos, pos) == ":", "expected :")
        pos = pos + 1
        obj[key] = value()
        skip()
        local d = text:sub(pos, pos)
        pos = pos + 1
        if d == "}" then return obj end
        assert(d == ",", "expected , or }")
      end
    elseif c == "[" then
      local arr = {}
      pos = pos + 1
      skip()
      if text:sub(pos, pos) == "]" then pos = pos + 1 return arr end
      while true do
        arr[#arr + 1] = value()
        skip()
        local d = text:sub(pos, pos)
        pos = pos + 1
        if d == "]" then return arr end
        assert(d == ",", "expected , or ]")
      end
    elseif c == '"' then
      return str()
    elseif text:sub(pos, pos + 3) == "true" then pos = pos + 4 return true
    elseif text:sub(pos, pos + 4) == "false" then pos = pos + 5 return false
    elseif text:sub(pos, pos + 3) == "null" then pos = pos + 4 return nil
    else
      local num = text:match("^-?%d+%.?%d*[eE]?[+-]?%d*", pos)
      assert(num and num ~= "", "unexpected character at " .. pos)
      pos = pos + #num
      return tonumber(num)
    end
  end
  local ok, result = pcall(value)
  if not ok then return nil, tostring(result) end
  skip()
  if pos <= #text then return nil, "trailing characters" end
  return result
end

-- install builds the stub, sets the global `rex`, and returns the control
-- table tests drive and inspect.
function M.install(profile_name)
  local profile = PROFILES[profile_name] or error("unknown profile " .. tostring(profile_name))
  local ctl = {
    profile = profile_name,
    loading = true, -- true until finish_load(): config top level has restricted APIs
    actions = {}, -- name -> definition
    logs = {}, -- {level, message}
    prompts = {}, -- the pick options received
    answers = {}, -- scripted pick answers, consumed in order
    execs = {}, -- {argv, opts}
    exec_handler = nil,
    windows = {}, -- ordered {window_id, label, blocks = {{block_id, label, layout}}}
    focused = {},
    queued = {},
    faults = {}, -- method name -> error string, makes that call return nil, err
    calls = {}, -- rex.call methods seen
    other_sessions = {}, -- {session_id, label, windows = view windows} on the same server
    attached = {}, -- sessions the connection is attached to
    remote_focused = {}, -- session/window pairs focused through rex.call
    splits = {}, -- new_split arguments received
  }
  local counter = 0
  local function next_id(prefix) counter = counter + 1 return prefix .. ":" .. counter end

  local function faulty(name)
    if ctl.faults[name] then return nil, ctl.faults[name] end
  end

  local api = {}

  function api.action(def)
    if not ctl.loading then fail("rex.action may only be called while the config loads") end
    if type(def) ~= "table" then fail("rex.action wants a table") end
    check_keys("rex.action", def, set({ "name", "title", "run", "description", "category", "keywords", "args" }))
    if type(def.name) ~= "string" or not def.name:match("^[a-z0-9_]+$") then fail("action name must match [a-z0-9_]+: " .. tostring(def.name)) end
    if ctl.actions[def.name] then fail("duplicate action " .. def.name) end
    if type(def.title) ~= "string" or def.title == "" then fail("action " .. def.name .. " needs a title") end
    if type(def.run) ~= "function" then fail("action " .. def.name .. " needs a run function") end
    for _, field in ipairs({ "description", "category", "keywords" }) do
      if def[field] ~= nil and type(def[field]) ~= "string" then fail("action " .. def.name .. " " .. field .. " must be a string") end
    end
    if def.args ~= nil then
      if type(def.args) ~= "table" then fail("action " .. def.name .. " args must be a table") end
      local types = set({ "string", "string?", "number", "number?", "boolean", "boolean?" })
      for key, kind in pairs(def.args) do
        if type(key) ~= "string" or not types[kind] then fail("action " .. def.name .. " has a bad arg declaration " .. tostring(key)) end
      end
    end
    ctl.actions[def.name] = def
  end

  function api.log(level, message)
    if not set({ "debug", "info", "warn", "error" })[level] then fail("rex.log: bad level " .. tostring(level)) end
    if type(message) ~= "string" then fail("rex.log wants a string message") end
    ctl.logs[#ctl.logs + 1] = { level, message }
  end

  function api.call(method, args)
    if type(method) ~= "string" then fail("rex.call wants a method name") end
    ctl.calls[#ctl.calls + 1] = method
    if ctl.loading then return nil, "rex.call is not available while the config loads" end
    validate(profile.api_revision, method, args, false)
    local err = select(2, faulty(method))
    if err then return nil, err end
    if method == "server.version" then
      return { api_revision = profile.api_revision, api_min_revision = 1 }
    elseif method == "session.list" then
      local sessions = { { session_id = "session:1", label = "Current" } }
      for _, other in ipairs(ctl.other_sessions) do sessions[#sessions + 1] = { session_id = other.session_id, label = other.label } end
      return { sessions = sessions }
    elseif method == "session.attach" then
      for _, other in ipairs(ctl.other_sessions) do
        if other.session_id == (args or {}).session_id then
          ctl.attached[other.session_id] = true
          return { view = { windows = other.windows }, client_id = "client:1" }
        end
      end
      return nil, "no such session"
    elseif method == "session.detach" then
      ctl.attached[(args or {}).session_id] = nil
      return {}
    elseif method == "session.focus_window" then
      local id = (args or {}).session_id
      -- A real server refuses calls into a session the connection is not attached to.
      if id ~= "session:1" and not ctl.attached[id] then return nil, "stream is not attached to session " .. tostring(id) end
      ctl.remote_focused[#ctl.remote_focused + 1] = id .. "/" .. (args or {}).window_id
      return {}
    end
    return nil, "unknown method " .. method
  end

  api.json = { decode = json_decode }

  api.layout = {
    block = function(spec)
      if type(spec) ~= "table" or type(spec.flavor) ~= "string" then fail("rex.layout.block needs a flavor") end
      check_keys("rex.layout.block", spec, set({ "flavor", "label", "options" }))
      local opts = spec.options or {}
      check_keys("terminal options", opts, set({ "command", "shell", "cwd", "initial_input", "exit", "interactive" }))
      if opts.command ~= nil then
        if type(opts.command) ~= "table" or #opts.command == 0 then fail("terminal command must be a non-empty list") end
        for _, part in ipairs(opts.command) do
          if type(part) ~= "string" then fail("terminal command parts must be strings") end
        end
      end
      if opts.exit ~= nil and (type(opts.exit) ~= "table" or type(opts.exit.on_completion) ~= "boolean") then
        fail("terminal exit must be { on_completion = boolean }")
      end
      return { flavor = spec.flavor, label = spec.label, options = opts }
    end,
  }

  function api.exec(argv, opts)
    if type(argv) ~= "table" or #argv == 0 then fail("rex.exec argv must be a non-empty list") end
    for _, part in ipairs(argv) do
      if type(part) ~= "string" then fail("rex.exec argv parts must be strings") end
    end
    opts = opts or {}
    check_keys("rex.exec", opts, set({ "cwd", "env", "clear_env", "stdin", "timeout" }))
    if opts.env ~= nil then
      for k, v in pairs(opts.env) do
        if type(k) ~= "string" or (type(v) ~= "string" and v ~= false) then fail("rex.exec env must map strings to strings") end
      end
    end
    ctl.execs[#ctl.execs + 1] = { argv = argv, opts = opts }
    if not ctl.exec_handler then fail("unscripted rex.exec: " .. table.concat(argv, " ")) end
    local result, err = ctl.exec_handler(argv, opts)
    if result == nil then return nil, err or "could not start" end
    return {
      code = result.code or 0, stdout = result.stdout or "", stderr = result.stderr or "",
      timed_out = result.timed_out or false,
      ok = (result.code or 0) == 0 and not result.timed_out,
    }
  end

  local function find_window(id)
    for _, w in ipairs(ctl.windows) do if w.window_id == id then return w end end
  end

  api.session = {
    new_window = function(args)
      local nilv, err = faulty("new_window")
      if err then return nilv, err end
      validate(profile.api_revision, "session.new_window", args, true)
      if type(args.window_label) ~= "string" then fail("new_window needs a window_label string") end
      if type(args.layout) ~= "table" or type(args.layout.flavor) ~= "string" then fail("new_window layout must come from rex.layout.block") end
      local window = { window_id = next_id("window"), label = args.window_label, blocks = {} }
      local block = { block_id = next_id("block"), label = args.layout.label or "", layout = args.layout }
      window.blocks[1] = block
      ctl.windows[#ctl.windows + 1] = window
      return { window_id = window.window_id, block_ids = { block.block_id } }
    end,
    view = function(args)
      validate(profile.api_revision, "session.view", args, true)
      local windows = {}
      for _, w in ipairs(ctl.windows) do
        local blocks = {}
        for _, b in ipairs(w.blocks) do blocks[#blocks + 1] = { block_id = b.block_id, label = b.label } end
        windows[#windows + 1] = { window_id = w.window_id, label = w.label, layers = { { blocks = blocks } } }
      end
      return { windows = windows }
    end,
    new_split = function(args)
      local nilv, err = faulty("new_split")
      if err then return nilv, err end
      validate(profile.api_revision, "session.new_split", args, true)
      if args.direction ~= "horizontal" and args.direction ~= "vertical" then fail("new_split direction must be horizontal or vertical") end
      if args.side ~= "before" and args.side ~= "after" then fail("new_split side must be before or after") end
      if args.ratio ~= nil and not (type(args.ratio) == "number" and args.ratio > 0 and args.ratio < 1) then fail("new_split ratio must be between 0 and 1") end
      if type(args.layout) ~= "table" or type(args.layout.flavor) ~= "string" then fail("new_split layout must come from rex.layout.block") end
      local target
      if args.anchor_id ~= nil then
        for _, w in ipairs(ctl.windows) do
          for _, b in ipairs(w.blocks) do if b.block_id == args.anchor_id then target = w end end
        end
        if not target then return nil, "no such anchor" end
      else
        target = ctl.windows[#ctl.windows]
        if not target then return nil, "no active window" end
      end
      local block = { block_id = next_id("block"), label = args.layout.label or "", layout = args.layout }
      target.blocks[#target.blocks + 1] = block
      ctl.splits[#ctl.splits + 1] = args
      return { block_ids = { block.block_id }, split_ids = { next_id("split") }, anchor_id = args.anchor_id }
    end,
    list_blocks = function(args)
      validate(profile.api_revision, "session.list_blocks", args, true)
      local blocks = {}
      for _, w in ipairs(ctl.windows) do
        for _, b in ipairs(w.blocks) do
          blocks[#blocks + 1] = { block_id = b.block_id, label = b.label, flavor = b.layout.flavor, placement = "placed", window_id = w.window_id }
        end
      end
      return { blocks = blocks }
    end,
    focus_block = function(args)
      validate(profile.api_revision, "session.focus_block", args, true)
      for _, w in ipairs(ctl.windows) do
        for _, b in ipairs(w.blocks) do
          if b.block_id == args.block_id then ctl.focused[#ctl.focused + 1] = args.block_id return {} end
        end
      end
      return nil, "no such block"
    end,
    focus_window = function(args)
      validate(profile.api_revision, "session.focus_window", args, true)
      if not find_window(args.window_id) then return nil, "no such window" end
      ctl.focused[#ctl.focused + 1] = args.window_id
      return {}
    end,
    set_block_label = function(args)
      validate(profile.api_revision, "session.set_block_label", args, true)
      if type(args.label) ~= "string" then fail("set_block_label needs a label") end
      for _, w in ipairs(ctl.windows) do
        for _, b in ipairs(w.blocks) do
          if b.block_id == args.block_id then b.label = args.label return {} end
        end
      end
      return nil, "no such block"
    end,
  }

  api.client = {
    queue = function(name)
      if type(name) ~= "string" then fail("rex.client.queue wants an action name") end
      ctl.queued[#ctl.queued + 1] = name
    end,
  }

  if profile.ui then
    api.ui = {
      pick = function(opts)
        if ctl.loading then return nil, "failed", "prompts fail while the config loads" end
        if type(opts) ~= "table" then fail("rex.ui.pick wants a table") end
        check_keys("rex.ui.pick", opts, set({ "title", "placeholder", "items", "multiple" }))
        if type(opts.title) ~= "string" or opts.title == "" then fail("pick needs a title") end
        if type(opts.items) ~= "table" or #opts.items == 0 then fail("pick needs at least one item") end
        if #opts.items > 2000 then fail("pick takes at most 2000 items, got " .. #opts.items) end
        local seen = {}
        for i, item in ipairs(opts.items) do
          check_keys("pick item " .. i, item, set({ "id", "title", "subtitle", "detail", "icon" }))
          if type(item.id) ~= "string" or item.id == "" then fail("pick item " .. i .. " needs an id") end
          if seen[item.id] then fail("pick has duplicate id " .. item.id) end
          seen[item.id] = true
          if type(item.title) ~= "string" or item.title == "" then fail("pick item " .. item.id .. " needs a title") end
          if item.icon ~= nil and not ICONS[item.icon] then fail("pick item " .. item.id .. " has unknown icon " .. tostring(item.icon)) end
        end
        ctl.prompts[#ctl.prompts + 1] = opts
        local answer = table.remove(ctl.answers, 1)
        if answer == nil then fail("unscripted prompt: " .. opts.title) end
        if type(answer) == "table" then return nil, answer.reason, answer.detail end
        if not seen[answer] then fail("scripted answer " .. tostring(answer) .. " is not among the items") end
        return answer
      end,
    }
  end

  rex = api -- absent APIs (rex.notify, rex.every, rex.on, ...) are plain nil, as on a real server

  function ctl.finish_load() ctl.loading = false end
  function ctl.last_log() return ctl.logs[#ctl.logs] end

  -- Run an action the way Rex does: ctx table, args table, result returned
  -- or the raised message. Returns ok, result_or_message.
  function ctl.run(name, args)
    local def = ctl.actions[name] or error("no action " .. name)
    return pcall(def.run, { session_id = "session:1", block_id = "block:0", origin = "test" }, args or {})
  end

  return ctl
end

return M
