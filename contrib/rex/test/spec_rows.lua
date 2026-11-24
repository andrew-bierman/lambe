-- Golden and unit tests for the pure row builders.
return function(T)
  local fixtures = require("fixtures")

  local function render_items(items)
    local lines = {}
    for _, it in ipairs(items) do
      lines[#lines + 1] = string.format("  %s | %s | %s | %s | %s", it.id, it.title, it.subtitle or "", it.icon or "", it.detail or "")
    end
    if #items == 0 then lines[1] = "  (none)" end
    return table.concat(lines, "\n")
  end

  local function render_all()
    local rows = require("lambe.rows")
    local out = {}
    local names = { "alive", "stopped_daemon", "legacy_no_alive", "unicode", "empty", "two_workflows" }
    for _, name in ipairs(names) do
      out[#out + 1] = "== " .. name .. " picker_items"
      out[#out + 1] = render_items(rows.picker_items(fixtures[name]))
      out[#out + 1] = "== " .. name .. " workflow_items"
      out[#out + 1] = render_items(rows.workflow_items(fixtures[name]))
    end
    out[#out + 1] = "== two_workflows picker_items(build)"
    out[#out + 1] = render_items(rows.picker_items(fixtures.two_workflows, "build"))
    out[#out + 1] = "== run_title"
    out[#out + 1] = "  " .. rows.run_title(fixtures.alive, "ac-1")
    out[#out + 1] = "  " .. rows.run_title(fixtures.alive, "ac-3")
    out[#out + 1] = "  " .. rows.run_title(fixtures.alive, "nope")
    out[#out + 1] = "  " .. rows.run_title(nil, "ac-1")
    return table.concat(out, "\n") .. "\n"
  end

  T.test("rows match the golden file", function()
    local path = T.here .. "/golden/rows.txt"
    local got = render_all()
    if os.getenv("UPDATE_GOLDEN") == "1" then
      local f = assert(io.open(path, "w"))
      f:write(got)
      f:close()
      return
    end
    local f = io.open(path, "r")
    T.ok(f, "missing " .. path .. "; run with UPDATE_GOLDEN=1")
    local want = f:read("*a")
    f:close()
    T.eq(got, want, "rows differ from golden (UPDATE_GOLDEN=1 rewrites it)")
  end)

  T.test("picker rows are capped at the 2000 rex.ui.pick limit", function()
    local rows = require("lambe.rows")
    T.eq(#rows.picker_items(fixtures.many(2500)), 2000, "capped")
    T.eq(#rows.picker_items(fixtures.many(10)), 10, "small lists untouched")
  end)

  T.test("picker ids are unique and live runs precede archived ones", function()
    local rows = require("lambe.rows")
    local items = rows.picker_items(fixtures.alive)
    local seen = {}
    for _, it in ipairs(items) do
      T.ok(not seen[it.id], "duplicate id " .. it.id)
      seen[it.id] = true
    end
    T.eq(items[1].id, "ac-1", "first row")
    T.ok(items[#items].subtitle:find("archived", 1, true), "last row is archived")
  end)
end
