-- Behavior of the installed actions against the strict stub.
return function(T)
  local rexstub = require("rexstub")
  local fixtures = require("fixtures")

  -- Encode a Lua value as JSON (the stub decodes `lambe logs -json` output).
  local function encode(v)
    local t = type(v)
    if t == "string" then
      return '"' .. v:gsub('[%c"\\]', function(c) return string.format("\\u%04x", c:byte()) end) .. '"'
    elseif t == "number" or t == "boolean" then
      return tostring(v)
    end
    if #v > 0 or next(v) == nil then
      local parts = {}
      for _, x in ipairs(v) do parts[#parts + 1] = encode(x) end
      return "[" .. table.concat(parts, ",") .. "]"
    end
    local keys = {}
    for k in pairs(v) do keys[#keys + 1] = k end
    table.sort(keys)
    local parts = {}
    for _, k in ipairs(keys) do parts[#parts + 1] = encode(k) .. ":" .. encode(v[k]) end
    return "{" .. table.concat(parts, ",") .. "}"
  end

  local BIN = "/opt/lambe"

  -- boot installs the stub, loads the modules like the shim does, and
  -- returns the stub control. `cfg` is lambe.config.lua's table.
  local function boot(profile, cfg, snapshot)
    T.fresh_modules() -- host.lua captures `rex` when first required
    local stub = rexstub.install(profile)
    stub.snapshot = snapshot or fixtures.alive
    stub.doctor = { code = 0, stdout = "doctor ok\n" }
    stub.exec_handler = function(argv)
      local last = argv[#argv]
      for i, a in ipairs(argv) do
        if a == "doctor" then return stub.doctor end
        if a == "logs" and argv[i + 1] == "-json" then return { code = 0, stdout = encode(stub.snapshot) } end
      end
      return nil, "unexpected command " .. tostring(last)
    end
    require("lambe.init").setup(cfg or { bin = BIN })
    stub.finish_load()
    return stub
  end

  local function window_command(stub, n) return stub.windows[n].blocks[1].layout.options.command end

  T.test("the shim loads, registers every action, and leaves no globals", function()
    local before = {}
    for k in pairs(_G) do before[k] = true end
    local stub = rexstub.install("p21")
    T.load_shim()
    stub.finish_load()
    for _, name in ipairs({ "lambe_dashboard", "lambe_demo", "lambe_launch", "lambe_open_run",
      "lambe_open_workflow_run", "lambe_needs_you", "lambe_switch_host", "lambe_check" }) do
      T.ok(stub.actions[name], "missing action " .. name)
      T.ok(stub.actions[name].description, name .. " has no description")
    end
    for k in pairs(_G) do
      T.ok(before[k] or k == "rex", "leaked global " .. tostring(k))
    end
  end)

  T.test("a broken install keeps one action that explains itself", function()
    local stub = rexstub.install("p21")
    local saved = package.path
    package.path = "/nonexistent/?.lua"
    local ok, err = pcall(T.load_shim, "/nonexistent")
    package.path = saved
    T.ok(ok, "shim must not raise: " .. tostring(err))
    stub.finish_load()
    local n = 0
    for _ in pairs(stub.actions) do n = n + 1 end
    T.eq(n, 1, "action count")
    local ran, message = stub.run("lambe_check")
    T.eq(ran, false, "check raises")
    T.contains(message, "lambe setup rex --apply", "tells the user what to do")
    T.eq(stub.last_log()[1], "error", "logged as error")
  end)

  T.test("Open Run with a picker opens the chosen run's window", function()
    local stub = boot("p21")
    stub.answers = { "ac-1" }
    local ok, err = stub.run("lambe_open_run")
    T.ok(ok, tostring(err))
    T.eq(stub.prompts[1].title, "Open Lambe run on mini · 1 need you", "picker title")
    T.eq(#stub.windows, 1, "windows")
    T.eq(stub.windows[1].label, "Lambe · ac-1", "window label contract")
    T.eq(stub.windows[1].blocks[1].label, "ac-1 · build · running", "pane title")
    T.eq(table.concat(window_command(stub, 1), " "), BIN .. " logs -issue ac-1 -follow", "command")
  end)

  T.test("config and state dir reach both the snapshot read and the terminal", function()
    local stub = boot("p21", { bin = BIN, config = "/etc/l.toml", state_dir = "/var/s" })
    stub.answers = { "ac-1" }
    T.ok(stub.run("lambe_open_run"))
    T.eq(stub.execs[1].opts.env.LAMBE_STATE_DIR, "/var/s", "exec env option")
    T.eq(table.concat(stub.execs[1].argv, " "), BIN .. " logs -json -config /etc/l.toml", "snapshot argv")
    T.eq(table.concat(window_command(stub, 1), " "),
      "env LAMBE_STATE_DIR=/var/s " .. BIN .. " logs -issue ac-1 -follow -config /etc/l.toml", "terminal argv")
  end)

  T.test("an open run window is reused and retitled unless the user renamed it", function()
    local stub = boot("p21")
    T.ok(stub.run("lambe_open_run", { issue = "ac-1" }))
    T.eq(#stub.windows, 1, "opened")
    stub.snapshot = { Alive = true, Runs = { { IssueID = "ac-1", Workflow = "build", Phase = "gating" } }, History = {} }
    T.ok(stub.run("lambe_open_run", { issue = "ac-1" }))
    T.eq(#stub.windows, 1, "reused, not duplicated")
    T.eq(stub.focused[1], stub.windows[1].window_id, "focused")
    T.eq(stub.windows[1].blocks[1].label, "ac-1 · build · gating", "retitled")
    stub.windows[1].blocks[1].label = "my name"
    stub.snapshot = { Alive = true, Runs = { { IssueID = "ac-1", Workflow = "build", Phase = "done" } }, History = {} }
    T.ok(stub.run("lambe_open_run", { issue = "ac-1" }))
    T.eq(stub.windows[1].blocks[1].label, "my name", "user rename kept")
  end)

  T.test("an issue argument never prompts and survives an unreadable snapshot", function()
    for _, profile in ipairs({ "p19", "p21" }) do
      local stub = boot(profile)
      stub.exec_handler = function() return nil, "ENOENT" end
      local ok, err = stub.run("lambe_open_run", { issue = "x-1" })
      T.ok(ok, tostring(err))
      T.eq(#stub.prompts, 0, profile .. " prompts")
      T.eq(stub.windows[1].label, "Lambe · x-1", profile .. " label")
      T.eq(stub.windows[1].blocks[1].label, "x-1", profile .. " title falls back to the id")
    end
  end)

  T.test("picker failures are raised with their reason; cancel is silent", function()
    local stub = boot("p21")
    stub.answers = { { reason = "failed", detail = "boom" } }
    local ok, err = stub.run("lambe_open_run")
    T.eq(ok, false, "raised")
    T.eq(err, "Lambe: run picker failed (failed: boom)", "message")
    stub.answers = { { reason = "no_client" } }
    ok, err = stub.run("lambe_open_run")
    T.contains(err, "no_client", "no_client surfaced")
    stub.answers = { { reason = "cancelled" } }
    ok, err = stub.run("lambe_open_run")
    T.ok(ok, "cancel must not raise: " .. tostring(err))
    T.eq(#stub.windows, 0, "nothing opened")
  end)

  T.test("no runs raises a hint instead of showing an empty picker", function()
    local stub = boot("p21", nil, fixtures.empty)
    local ok, err = stub.run("lambe_open_run")
    T.eq(ok, false, "raised")
    T.contains(err, "no runs yet", "message")
    T.eq(#stub.prompts, 0, "no prompt")
  end)

  T.test("servers below API revision 17 say so once and open the dashboard", function()
    for profile, revision in pairs({ p16 = 13, p19 = 14 }) do
      local stub = boot(profile)
      for _, name in ipairs({ "lambe_open_run", "lambe_launch", "lambe_open_workflow_run", "lambe_needs_you" }) do
        local ok, message = stub.run(name)
        T.ok(ok, tostring(message))
        T.contains(message, "preview.21", profile .. " " .. name)
        T.contains(message, "API revision " .. revision, profile .. " " .. name)
        T.contains(message, "opened the dashboard", profile .. " " .. name)
      end
      T.eq(#stub.windows, 4, profile .. " dashboards")
      T.eq(stub.windows[1].label, "Lambe dashboard", profile .. " label")
      local warns = 0
      for _, entry in ipairs(stub.logs) do if entry[1] == "warn" then warns = warns + 1 end end
      T.eq(warns, 1, profile .. " logged once")
    end
  end)

  T.test("choose dashboard picks live or demo, demo takes no config", function()
    local stub = boot("p21", { bin = BIN, config = "/c.toml", state_dir = "/s" })
    stub.answers = { "demo", "live" }
    T.ok(stub.run("lambe_launch"))
    T.eq(stub.windows[1].label, "Lambe demo", "demo label")
    T.eq(table.concat(window_command(stub, 1), " "), BIN .. " tui -demo", "demo command")
    T.ok(stub.run("lambe_launch"))
    T.eq(table.concat(window_command(stub, 2), " "), "env LAMBE_STATE_DIR=/s " .. BIN .. " tui -config /c.toml", "live command")
  end)

  T.test("workflow picker narrows the run picker; a single workflow skips it", function()
    local stub = boot("p21", nil, fixtures.two_workflows)
    stub.answers = { "build", "a-3" }
    T.ok(stub.run("lambe_open_workflow_run"))
    T.eq(stub.prompts[1].title, "Lambe workflow", "first prompt")
    T.contains(stub.prompts[2].title, "build", "second prompt")
    T.eq(#stub.prompts[2].items, 2, "only build runs")
    T.eq(stub.windows[1].label, "Lambe · a-3", "opened")
    local single = boot("p21", nil, fixtures.stopped_daemon)
    single.answers = { "ac-9" }
    T.ok(single.run("lambe_open_workflow_run"))
    T.eq(#single.prompts, 1, "one prompt")
  end)

  T.test("exec failures are explained", function()
    local stub = boot("p21")
    stub.exec_handler = function() return nil, "ENOENT" end
    local ok, err = stub.run("lambe_open_run")
    T.contains(err, "could not start", "never ran")
    T.contains(err, "ENOENT", "reason")
    stub.exec_handler = function() return { code = 127, stderr = "lambe: not found\n" } end
    ok, err = stub.run("lambe_open_run")
    T.contains(err, "login shell's PATH", "exit 127")
    stub.exec_handler = function() return { code = -1, timed_out = true } end
    ok, err = stub.run("lambe_open_run")
    T.contains(err, "timed out", "timeout")
    stub.exec_handler = function() return { code = 0, stdout = "not json" } end
    ok, err = stub.run("lambe_open_run")
    T.contains(err, "printed no snapshot", "bad json")
  end)

  T.test("a bare binary name runs through the login shell, an absolute path directly", function()
    local stub = boot("p21", {})
    local host = require("lambe.host")
    local env = { SHELL = "/bin/zsh", HOME = "/Users/x" }
    host.getenv = function(name) return env[name] end
    stub.answers = { "ac-1" }
    T.ok(stub.run("lambe_open_run"))
    T.eq(table.concat(stub.execs[1].argv, " "), '/bin/zsh -l -c exec "$@" lambe lambe logs -json', "wrapped")
    env.SHELL = nil
    stub.answers = { "ac-1" }
    T.ok(stub.run("lambe_open_run"))
    T.eq(stub.execs[2].argv[1], "/bin/zsh", "macOS fallback is zsh, not sh")
    env.HOME = "/home/x"
    stub.answers = { "ac-1" }
    T.ok(stub.run("lambe_open_run"))
    T.eq(stub.execs[3].argv[1], "/bin/sh", "other platforms fall back to sh")
    env.LAMBE_REX_BIN = "/abs/lambe"
    stub.answers = { "ac-1" }
    T.ok(stub.run("lambe_open_run"))
    T.eq(stub.execs[4].argv[1], "/abs/lambe", "absolute path runs directly")
  end)

  T.test("Check Setup returns a machine-readable report and fails when doctor fails", function()
    local stub = boot("p21")
    local ok, report = stub.run("lambe_check")
    T.ok(ok, tostring(report))
    T.eq(report.api_revision, 18, "api revision")
    T.eq(report.pickers, true, "pickers")
    T.eq(report.bin, BIN, "bin")
    T.eq(report.doctor.output, "doctor ok\n", "doctor output")
    stub.doctor = { code = 2, stdout = "", stderr = "no daemon\n" }
    local ok2, err = stub.run("lambe_check")
    T.eq(ok2, false, "raised")
    T.contains(err, "no daemon", "doctor output in the error")
    local old = boot("p19")
    local ok3, report3 = old.run("lambe_check")
    T.ok(ok3, tostring(report3))
    T.eq(report3.pickers, false, "pickers unavailable on p19")
    T.contains(report3.picker_note, "preview.21", "why")
  end)

  T.test("Needs You lists only runs waiting on a human and opens the chosen one", function()
    local stub = boot("p21", nil, fixtures.attention)
    stub.answers = { "n-5" }
    local ok, err = stub.run("lambe_needs_you")
    T.ok(ok, tostring(err))
    T.eq(stub.prompts[1].title, "Lambe runs that need you on mini", "title")
    local ids = {}
    for _, it in ipairs(stub.prompts[1].items) do ids[#ids + 1] = it.id end
    T.eq(table.concat(ids, ","), "n-2,n-3,n-4,n-5", "only attention rows")
    T.eq(stub.windows[1].label, "Lambe · n-5", "opened")
  end)

  T.test("Needs You with nothing to do says so without a picker or an error", function()
    local stub = boot("p21", nil, fixtures.alive_quiet)
    local ok, message = stub.run("lambe_needs_you")
    T.ok(ok, tostring(message))
    T.eq(message, "Nothing needs you right now.", "message")
    T.eq(#stub.prompts, 0, "no prompt")
  end)

  T.test("Needs You degrades like the other pickers on old servers", function()
    local stub = boot("p19", nil, fixtures.attention)
    local ok, message = stub.run("lambe_needs_you")
    T.ok(ok, tostring(message))
    T.contains(message, "preview.21", "message")
    T.eq(stub.windows[1].label, "Lambe dashboard", "dashboard")
  end)

  T.test("empty states explain what to do", function()
    local stub = boot("p21", nil, fixtures.stopped_daemon)
    stub.snapshot = { Alive = false, Runs = {}, History = {} }
    local ok, err = stub.run("lambe_open_run")
    T.eq(ok, false, "raised")
    T.contains(err, "daemon is stopped", "stopped daemon")
    stub.snapshot = fixtures.alive
    ok, err = stub.run("lambe_open_run", { workflow = "nope" })
    T.eq(ok, false, "raised")
    T.contains(err, "no runs for workflow `nope`", "workflow")
  end)

  T.test("output tabs stay open after the command exits; the dashboard does not", function()
    local stub = boot("p21")
    T.ok(stub.run("lambe_open_run", { issue = "ac-1" }))
    T.ok(stub.run("lambe_dashboard"))
    local run_opts = stub.windows[1].blocks[1].layout.options
    T.eq(run_opts.exit.on_completion, false, "run tab keeps its output")
    T.eq(stub.windows[2].blocks[1].layout.options.exit, nil, "dashboard closes on quit")
  end)

  T.test("split placement opens beside the invoking block and reuses the pane", function()
    local stub = boot("p21")
    T.ok(stub.run("lambe_dashboard"))
    local anchor = stub.windows[1].blocks[1].block_id
    local function run(args)
      local def = stub.actions.lambe_open_run
      return pcall(def.run, { session_id = "session:1", block_id = anchor }, args)
    end
    T.ok(run({ issue = "ac-1", placement = "split" }))
    T.eq(#stub.windows, 1, "no new window")
    T.eq(#stub.windows[1].blocks, 2, "one new pane")
    T.eq(stub.splits[1].anchor_id, anchor, "anchored on the invoking block")
    T.eq(stub.splits[1].direction, "horizontal", "side by side")
    T.eq(stub.windows[1].blocks[2].label, "ac-1 · build · running", "pane title")
    stub.snapshot = { Alive = true, Runs = { { IssueID = "ac-1", Workflow = "build", Phase = "gating" } }, History = {} }
    T.ok(run({ issue = "ac-1", placement = "split" }))
    T.eq(#stub.windows[1].blocks, 2, "pane reused")
    T.eq(stub.focused[#stub.focused], stub.windows[1].blocks[2].block_id, "pane focused")
    T.eq(stub.windows[1].blocks[2].label, "ac-1 · build · gating", "pane retitled")
  end)

  T.test("placement comes from the argument, then lambe.config.lua, and is validated", function()
    local stub = boot("p21", { bin = BIN, placement = "split" })
    T.ok(stub.run("lambe_dashboard"))
    local anchor = stub.windows[1].blocks[1].block_id
    local def = stub.actions.lambe_open_run
    T.ok(pcall(def.run, { session_id = "session:1", block_id = anchor }, { issue = "a-1" }))
    T.eq(#stub.splits, 1, "config default is split")
    T.ok(pcall(def.run, { session_id = "session:1", block_id = anchor }, { issue = "a-2", placement = "tab" }))
    T.eq(stub.windows[2].label, "Lambe · a-2", "argument wins")
    local ok, err = stub.run("lambe_open_run", { issue = "a-3", placement = "float" })
    T.eq(ok, false, "invalid placement raised")
    T.contains(err, "placement must be", "message")
  end)

  T.test("a run open in another session is found, not duplicated", function()
    local stub = boot("p21")
    stub.other_sessions = { {
      session_id = "session:2", label = "Agents",
      windows = { { window_id = "window:r", label = "Lambe · ac-1", layers = { { blocks = { { block_id = "block:r", label = "ac-1" } } } } } },
    } }
    local ok, message = stub.run("lambe_open_run", { issue = "ac-1" })
    T.ok(ok, tostring(message))
    T.eq(#stub.windows, 0, "nothing duplicated")
    T.eq(stub.remote_focused[1], "session:2/window:r", "focused where it lives")
    T.contains(message, "already open in session `Agents`", "says where")
    T.eq(next(stub.attached), nil, "detached again")
  end)

  T.test("a missing rex.ui on a new-enough server is explained, not blamed on the server", function()
    local stub = boot("p21")
    rex.ui = nil -- a rex CLI older than the server runs the script
    local ok, message = stub.run("lambe_open_run")
    T.ok(ok, tostring(message))
    T.contains(message, "this Lua runtime has no rex.ui", "message")
    T.contains(message, "API revision 18", "message")
  end)

  T.test("Switch Host asks the app for its host picker", function()
    local stub = boot("p21")
    T.ok(stub.run("lambe_switch_host"))
    T.eq(stub.queued[1], "client.host.switch", "queued")
  end)
end
