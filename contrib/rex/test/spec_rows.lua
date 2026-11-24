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
    local names = { "alive", "stopped_daemon", "legacy_no_alive", "unicode", "empty", "two_workflows", "attention" }
    for _, name in ipairs(names) do
      out[#out + 1] = "== " .. name .. " picker_items"
      out[#out + 1] = render_items(rows.picker_items(fixtures[name]))
      out[#out + 1] = "== " .. name .. " workflow_items"
      out[#out + 1] = render_items(rows.workflow_items(fixtures[name]))
    end
    out[#out + 1] = "== two_workflows picker_items(build)"
    out[#out + 1] = render_items(rows.picker_items(fixtures.two_workflows, "build"))
    out[#out + 1] = "== attention picker_items(only attention)"
    out[#out + 1] = render_items(rows.picker_items(fixtures.attention, nil, true))
    out[#out + 1] = "== attention picker_items(review)"
    out[#out + 1] = render_items(rows.picker_items(fixtures.attention, "review"))
    out[#out + 1] = "== empty_message"
    out[#out + 1] = "  " .. rows.empty_message(fixtures.empty)
    out[#out + 1] = "  " .. rows.empty_message(fixtures.stopped_daemon)
    out[#out + 1] = "  " .. rows.empty_message(fixtures.empty, "build")
    out[#out + 1] = "  " .. rows.empty_message(fixtures.empty, nil, true)
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

  T.test("needs-you runs sort first, whether from Attention or the phase", function()
    local rows = require("lambe.rows")
    local items = rows.picker_items(fixtures.attention)
    local order = {}
    for _, it in ipairs(items) do order[#order + 1] = it.id end
    T.eq(table.concat(order, ","), "n-2,n-3,n-4,n-5,n-1,n-6", "order")
    T.eq(rows.attention_count(fixtures.attention), 4, "count")
    T.contains(items[1].subtitle, "needs you: no activity", "explicit reason")
    T.contains(items[1].subtitle, "14m", "age")
  end)

  T.test("distinct issue counts match the rows a workflow opens", function()
    local rows = require("lambe.rows")
    local counts = {}
    for _, it in ipairs(rows.workflow_items(fixtures.attention)) do counts[it.id] = it.subtitle end
    T.contains(counts.build, "4 runs", "build counts n-1..n-4 once each")
    T.eq(#rows.picker_items(fixtures.attention, "build"), 4, "build rows")
  end)

  T.test("fresh rows hide attempt 0 and turn 0 noise; a title equal to the id has no detail", function()
    local rows = require("lambe.rows")
    local items = rows.picker_items(fixtures.legacy_no_alive)
    T.ok(not items[1].subtitle:find("attempt", 1, true), items[1].subtitle)
    T.ok(not items[1].subtitle:find("turn", 1, true), items[1].subtitle)
    local gating = rows.picker_items(fixtures.alive)[3]
    T.eq(gating.title, "ac-2", "title falls back to id")
    T.eq(gating.detail, nil, "no duplicate detail")
  end)

  T.test("picker ids are unique and needs-you then live runs come first", function()
    local rows = require("lambe.rows")
    local items = rows.picker_items(fixtures.alive)
    local seen = {}
    for _, it in ipairs(items) do
      T.ok(not seen[it.id], "duplicate id " .. it.id)
      seen[it.id] = true
    end
    T.eq(items[1].id, "ac-3", "needs-you row first")
    T.eq(items[2].id, "ac-1", "then live")
    T.ok(items[1].subtitle:find("needs you", 1, true), "first row says why")
  end)
end
