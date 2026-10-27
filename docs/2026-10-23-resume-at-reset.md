# Resume at the usage limit's reset

When the plan's usage limit stops a call, Claude Code ends it with a result
whose text says when the limit resets, as `You've hit your session limit ·
resets 4am (America/New_York)`. Until now opt-chat posted "No reply" and waited
for the user; a subagent's report said "failed".

Now, with `-resume-at-reset` (on), `limitresume.go`:

- counts only the plan's usage limit (`limitText`): session, weekly, 5-hour,
  Opus, Sonnet. Context, rate-limit (429), spend, credit and overload errors
  never wait for a reset;
- prefers Claude Code's `rate_limit_event` with status `rejected` and its
  `resetsAt`; an event with `errorCode` (`credits_required`) is not a limit.
  The message wording is the backup;
- reads the reset time from the message (`limitReset`: a time of day with its
  zone, a weekday and time such as `resets Mon 12:00am`, a date and time,
  "in 2 hours", or the older `|<unix seconds>`); a time of day that just
  passed means now, not tomorrow; with none, the latest used-up window's
  reset, else an hour on;
- keeps each caller's own reset time, and drops subagents that have finished;
- keeps each stopped caller's resume time in `<dir>/resume.json` (Opt's turn
  under "", each subagent under its name); it outlasts a restart, and one
  missed while down goes out as opt-chat starts;
- `-reset-grace` (90s) after a caller's reset, sends a report, `[opt-chat]
  usage limit reset at <time>; stopped by it: ...`, for the callers due then,
  which starts a turn, and tells each of those subagents to check its state
  and go on, as `tell` does.

A limit that hits the same caller again moves its time later, never sooner. A time already
past waits `5 min × (resumes in a row + 1)`. After six resumes in a row the
limit stops resuming on its own; a turn or subagent that ends without the
limit starts the count over.

Only plan limits are covered. With an API key there is no plan limit event,
so nothing resumes by itself.
