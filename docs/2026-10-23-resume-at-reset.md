# Resume at the usage limit's reset

When the plan's usage limit stops a call, Claude Code ends it with a result
whose text says when the limit resets, as `You've hit your session limit ·
resets 4am (America/New_York)`. Until now opt-chat posted "No reply" and waited
for the user; a subagent's report said "failed".

Now, with `-resume-at-reset` (on), `limitresume.go`:

- reads the reset time from the message (`limitReset`: a time of day with its
  zone, a date and time, "in 2 hours", or the older `|<unix seconds>`); with
  none, the latest used-up window's reset, else an hour on;
- keeps one pending resume in `<dir>/resume.json`: when, whether Opt's turn
  was stopped, and which subagents; it outlasts a restart, and one missed
  while down goes out as opt-chat starts;
- `-reset-grace` (90s) after the reset, sends one report, `[opt-chat] usage
  limit reset at <time>; stopped by it: ...`, which starts a turn, and tells
  each stopped subagent to check its state and go on, as `tell` does.

A limit that hits again moves the time later, never sooner. A time already
past waits `5 min × (resumes in a row + 1)`. After six resumes in a row the
limit stops resuming on its own; a turn or subagent that ends without the
limit starts the count over.
