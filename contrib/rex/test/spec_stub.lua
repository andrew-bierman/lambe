-- The stub must reject what a real Rex rejects, or green tests prove nothing.
return function(T)
  local rexstub = require("rexstub")

  local function rejects(fn, text, what)
    local ok, err = pcall(fn)
    T.eq(ok, false, what .. " should be rejected")
    T.contains(err, text, what)
  end

  T.test("stub enforces action definitions", function()
    local stub = rexstub.install("p21")
    rejects(function() rex.action{ name = "Bad-Name", title = "t", run = function() end } end, "[a-z0-9_]", "bad name")
    rex.action{ name = "good", title = "t", run = function() end }
    rejects(function() rex.action{ name = "good", title = "t", run = function() end } end, "duplicate", "duplicate")
    rejects(function() rex.action{ name = "x", run = function() end } end, "title", "missing title")
    rejects(function() rex.action{ name = "y", title = "t" } end, "run function", "missing run")
    stub.finish_load()
    rejects(function() rex.action{ name = "late", title = "t", run = function() end } end, "config loads", "late action")
  end)

  T.test("stub enforces picker shapes and answers", function()
    local stub = rexstub.install("p21")
    stub.finish_load()
    local items = { { id = "a", title = "A" }, { id = "b", title = "B" } }
    rejects(function() rex.ui.pick{ title = "t", items = { { id = "a", title = "A" }, { id = "a", title = "A2" } } } end, "duplicate id", "duplicate id")
    rejects(function() rex.ui.pick{ title = "t", items = { { id = "a" } } } end, "title", "missing item title")
    rejects(function() rex.ui.pick{ title = "t", items = { { id = "a", title = "A", icon = "nope" } } } end, "icon", "bad icon")
    rejects(function() rex.ui.pick{ items = items } end, "title", "missing title")
    local many = {}
    for i = 1, 2001 do many[i] = { id = "i" .. i, title = "x" } end
    rejects(function() rex.ui.pick{ title = "t", items = many } end, "2000", "item cap")
    rejects(function() rex.ui.pick{ title = "t", items = items } end, "unscripted", "unscripted prompt")
    stub.answers = { "a", { reason = "cancelled" } }
    T.eq(rex.ui.pick{ title = "t", items = items }, "a", "scripted answer")
    local chosen, reason = rex.ui.pick{ title = "t", items = items }
    T.eq(chosen, nil, "cancel")
    T.eq(reason, "cancelled", "reason")
  end)

  T.test("prompts fail while the config loads, and rex.call is unavailable", function()
    local stub = rexstub.install("p21")
    local chosen, reason = rex.ui.pick{ title = "t", items = { { id = "a", title = "A" } } }
    T.eq(chosen, nil, "no answer at load")
    T.eq(reason, "failed", "reason at load")
    T.eq(rex.call("server.version"), nil, "rex.call at load")
    stub.finish_load()
    T.eq(rex.call("server.version").api_revision, 18, "rex.call later")
  end)

  T.test("version profiles gate rex.ui and APIs the platform lacks", function()
    for profile, want in pairs({ p16 = 13, p19 = 14, p21 = 18 }) do
      local stub = rexstub.install(profile)
      stub.finish_load()
      T.eq(rex.call("server.version").api_revision, want, profile .. " revision")
      T.eq(rex.ui ~= nil, profile == "p21", profile .. " rex.ui")
      T.eq(rex.notify, nil, profile .. " has no rex.notify")
      T.eq(rex.every, nil, profile .. " has no rex.every")
    end
  end)

  T.test("stub validates exec and layout arguments", function()
    local stub = rexstub.install("p21")
    stub.finish_load()
    rejects(function() rex.exec({}) end, "non-empty", "empty argv")
    rejects(function() rex.exec({ "x", 1 }) end, "strings", "non-string argv")
    rejects(function() rex.exec({ "x" }, { shell = true }) end, "unknown field", "unknown exec option")
    rejects(function() rex.exec({ "x" }) end, "unscripted", "unscripted exec")
    rejects(function() rex.layout.block{ flavor = "f", options = { command = { 1 } } } end, "strings", "bad command")
    rejects(function() rex.layout.block{ flavor = "f", options = { bogus = 1 } } end, "unknown field", "bad terminal option")
  end)

  T.test("stub validates Rex method arguments against the captured schemas", function()
    for _, profile in ipairs({ "p16", "p19", "p21" }) do
      local stub = rexstub.install(profile)
      stub.finish_load()
      local block = rex.layout.block{ flavor = "com.superlogical.terminal.shell" }
      rejects(function() rex.session.new_window{ window_label = "w", layout = block, bogus = 1 } end, "unknown field `bogus`", profile .. " extra field")
      rejects(function() rex.session.new_window{ window_label = 3, layout = block } end, "must be string", profile .. " wrong type")
      rejects(function() rex.session.new_split{ direction = "diagonal", side = "after", layout = block } end, "must be one of", profile .. " bad enum")
      rejects(function() rex.session.new_split{ direction = "horizontal", layout = block } end, "missing required field `side`", profile .. " missing field")
      rejects(function() rex.call("session.attach", {}) end, "missing required field `session_id`", profile .. " rex.call needs session_id")
      rejects(function() rex.session.focus_block{} end, "missing required field `block_id`", profile .. " focus_block")
      local result = rex.session.new_window{ window_label = "w", layout = block, focus = false }
      T.ok(result.window_id, profile .. " valid new_window")
    end
  end)
end