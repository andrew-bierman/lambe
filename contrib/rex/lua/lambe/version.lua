-- Plugin version and the Rex API revisions features need. VERSION is
-- stamped into the managed header by `lambe setup rex`.
return {
  VERSION = 2,
  MIN_API = 13, -- oldest Rex API revision the dashboard actions are known to work on
  UI_API = 17, -- rex.ui (pickers) needs API revision 17, Rex preview.21
}
