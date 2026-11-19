local function open_lambe(mode)
  local command = { "lambe", "tui" }
  local title = "Lambe dashboard"
  if mode == "demo" then
    command[3] = "-demo"
    title = "Lambe demo"
  end
  rex.session.new_window{
    window_label = title,
    layout = rex.layout.block{
      flavor = "com.superlogical.terminal.shell",
      options = { command = command, interactive = true },
    },
  }
end

rex.action{
  name = "lambe_dashboard",
  title = "Lambe: Open Dashboard",
  run = function() open_lambe("live") end,
}

rex.action{
  name = "lambe_demo",
  title = "Lambe: Open Demo",
  run = function() open_lambe("demo") end,
}

if rex.ui and rex.ui.pick then
  rex.action{
    name = "lambe_launch",
    title = "Lambe: Choose Dashboard…",
    args = { mode = "string?" },
    run = function(_, args)
      local mode = args.mode
      if mode == nil then
        mode = rex.ui.pick{
          title = "Open Lambe",
          items = {
            { id = "live", title = "Your dashboard", subtitle = "Use your default Lambe configuration", icon = "terminal" },
            { id = "demo", title = "Explore the demo", subtitle = "Use sample runs without writing to Beads", icon = "star" },
          },
        }
      end
      if mode == "live" or mode == "demo" then open_lambe(mode) end
    end,
  }
end
