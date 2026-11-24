return function(T)
  T.test("fail raises the message with the reason, without a position", function()
    local errors = require("lambe.errors")
    local ok, err = pcall(errors.fail, "boom", "why")
    T.eq(ok, false, "raised")
    T.eq(err, "Lambe: boom (why)", "message")
    ok, err = pcall(errors.fail, "plain")
    T.eq(err, "Lambe: plain", "no reason")
  end)

  T.test("picked passes choices, keeps cancellation silent, raises the rest", function()
    local errors = require("lambe.errors")
    T.eq(errors.picked("a", nil, nil, "x"), "a", "choice")
    T.eq(errors.picked(nil, "cancelled", nil, "x"), nil, "cancelled")
    T.eq(errors.picked(nil, nil, nil, "x"), nil, "no reason")
    for _, reason in ipairs({ "timeout", "no_client", "unsupported", "busy", "failed" }) do
      local ok, err = pcall(errors.picked, nil, reason, "detail", "run picker")
      T.eq(ok, false, reason .. " raised")
      T.eq(err, "Lambe: run picker failed (" .. reason .. ": detail)", reason)
    end
  end)
end
