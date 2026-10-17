# Hot reload, Stage 2: keep the calls alive across a reload (to do)

Written 2026-10-15. Not built. Stage 1 is built: `/reload` and SIGUSR1
build, wait for the chat to be idle, stop every call and exec in place, and
every start resumes the subagents cut off (see the guide's Reloading,
`reload.go`, `recoverAgents` and `resumeCut` in `resume.go`). This note is
what Stage 2 would take, from the research of 15 October (the
[OptHotReload] report) and what Stage 1 taught.

## Goal

A reload loses nothing that is running: each subagent's `claude` process
goes on through the exec, mid-tool, and the new opt-chat takes it over. No
tool call is cut, so none is redone or half-done, and no resume costs a
cache write.

Stage 1 loses, per reload:

- the tool call each subagent at work was in (a 5-minute benchmark starts
  again; a remote job over ssh may run on, unseen);
- the cache of each resumed session (one write of its whole context);
- each subagent at rest (its call ends; a `tell` resumes it later, also a
  cache write);
- a question waiting on the user;
- a compaction underway (the next run builds it again).

Build Stage 2 only if these hurt in use: for example long benchmarks cut by
reloads, or reloads many times a day while subagents work.

## Why it can work: exec in place keeps pipes and children

`syscall.Exec` replaces the program, not the process. The pid stays, so the
`claude` processes stay its children and can still be reaped, and any file
descriptor without close-on-exec stays open. A proof of concept on macOS
(15 October, /tmp/opt-hotreload/execpoc/main.go, scratch):

- generation 1 starts `cat` with stdin and stdout pipes, writes a line,
  clears FD_CLOEXEC on both pipe fds, and execs itself with their numbers
  in the environment;
- generation 2, the same pid, adopts them with `os.NewFile` and the child
  with `os.FindProcess`, writes a second line, reads both back, and reaps
  the child with `exit status 0`.

Not yet tried on Linux, where it should work the same.

## Design

### Before the exec (old process)

1. Hold new work off, as Stage 1 does: wait for the chat to be idle, set
   `h.quitting`, `c.compact = nil`, refuse `begin`.
2. Compactions, side questions and the control call: let them end, or stop
   them as Stage 1 does. They are cheap to redo. Keeping only subagents
   alive keeps the handoff small.
3. MCP server: keep its address. Today `serveMCP` listens on
   `127.0.0.1:0` behind a random secret path (mcp.go:18-23), and the URL
   is baked into each child's `--mcp-config`. A child kept alive would hold
   a dead URL. Either pass the listener fd through the exec (clear
   FD_CLOEXEC, `net.FileListener` after), or save port and secret like
   `web.json` and listen on them again. Passing the fd is better: the port
   can't be taken meanwhile, and connections queue in the backlog instead
   of being refused.
4. Web listener: optional. It already comes back on the same port and key
   (`web.json`), and pages reconnect. Passing its fd too would avoid the
   short refusal window.
5. For each subagent's proc, quiesce its reader:
   - Replace `bufio.Scanner` (claude.go `next`) with a `bufio.Reader`,
     since a Scanner can't hand over what it has buffered. Stop the reader
     goroutine between lines (`SetReadDeadline(now)` on the pipe, then wait
     for it), and keep `r.Peek(r.Buffered())`: bytes read but not yet
     parsed. They go into the handoff.
   - Only then call `File.Fd()`, which sets the fd blocking and breaks
     deadlines (`os.File.Fd` docs). After it, nothing more is read in the
     old process.
   - Clear FD_CLOEXEC on stdin and stdout (`syscall.CloseOnExec` is
     one-way, so use `fcntl(F_SETFD, 0)` via `unix.FcntlInt` or
     `syscall.Syscall`).
   - stderr: today it is captured into a `strings.Builder` read at the
     end. Pass it as well, or move it to a file so there is nothing to hand
     over.
6. Write `<dir>/handoff.json`, fsynced, one entry per subagent:
   - name, pid, stdin fd, stdout fd (and stderr's);
   - leftover bytes (base64);
   - its state: working, resting, or compacting at rest; its
     `rest.until`, `rest.pending`, `rest.compacted`, `rest.task`;
   - its `size` (context tokens) and `window`;
   - its loop: `loopState` (turns, again, compacting, next) and whether
     its hook was mid-run (see Risks);
   - its tells held (`a.held[name]`);
   - its questions waiting (`a.asks` for it): the raw `control_request`
     events, to show again;
   - its spend so far (`p.spent`) and `p.who`, `p.prefix`;
   - its control calls in flight (`p.calls`): their request ids. An
     answer that comes after the exec must reach the new process, which
     can't wait on the old channel. Simplest: drain them first (stop
     sending control requests, wait for the answers), or answer them as
     errors.
7. Exec, with the handoff's path in the environment.

### After the exec (new process)

1. Read and delete `handoff.json` (deleting it first makes a crash during
   adoption fall back to Stage 1's resume, never adopt twice).
2. Listen on the passed MCP fd; serve the web as now.
3. For each entry, `os.NewFile` on its fds, `os.FindProcess(pid)`, and
   build a `proc`: a Reader on stdout, preceded by the leftover bytes
   (`io.MultiReader`), stdin as its writer, and `cmd`-like waiting
   (`Process.Wait`), with the same `done` (children map, `a.calls`,
   `a.live`), `ctx` and cancel (SIGTERM to the group, WaitDelay, then
   SIGKILL), since `exec.Cmd` can't be rebuilt around an existing process.
4. Rebuild `a.running`, `a.resting` (with new rest timers from `until`),
   `a.halts`, `a.held`, the rail's `subs`, and the asks (shown to the user
   again).
5. Enter the subagent's event loop. `runAgent` is one long function today:
   split it into its start (spawn or resume, the first messages) and its
   loop over events, with the loop's state in a struct both can fill. This
   refactor is most of the work, and worth doing first, on its own, with
   the tests green.
6. Agents not in the handoff are resumed as in Stage 1.

### What it keeps and what it doesn't

Keeps: every subagent's process, mid-tool, its context and cache, its
loop, its held tells, its questions. Doesn't keep: the chat's turn (drained
first, as now), compactions, side questions, the control call (all cheap to
redo), and anything if the exec fails (then the old process must adopt its
own handoff again, or fall back to Stage 1's resume).

## Size

About 450 to 600 lines across claude.go (proc from fds, Reader instead of
Scanner, waiting without exec.Cmd), agents.go (runAgent split, snapshot and
adoption), loop.go (loop state out and in), mcp.go (listener from an fd),
perms.go (asks out and in) and main.go/reload.go (handoff file, exec env),
plus about 300 lines of tests.

## Risks

- Snapshot drift. Every field later added to `Agent`, `rest`, `proc` or
  `loopState` must also go into the handoff, or it is silently lost on a
  reload. A test that fills every field and checks it survives a
  snapshot/adopt round trip catches most of it (a reflection check that
  every field is either handed over or listed as dropped).
- Bytes in flight. A line half-read when the reader stops must not be lost
  or doubled: leftover bytes plus a reader stopped between lines.
- `File.Fd()` sets blocking mode and drops deadlines, so it must come after
  the reader is stopped, and the new process must set non-blocking mode
  again (`syscall.SetNonblock`) to get deadlines and the poller back.
- A loop hook mid-run during the reload. Its process is opt-chat's child
  with its own group; adopting it is more work than it is worth. Kill it
  and run it again after the exec: hooks should be safe to run twice
  (autoresearch's is).
- Control requests in flight (permission answers, mode switches, MCP
  status) across the exec: drain them first.
- MCP calls from a subagent during the gap: refused for the second or so
  the exec takes (or queued, if the listener fd is passed). Unverified:
  whether Claude Code marks an HTTP MCP server failed for the rest of the
  session after a refused call. Passing the listener fd avoids the
  question.
- EPIPE: if the old process dies without handing over (a crash mid-reload),
  each child loses its stdout reader. Unverified whether Claude Code dies
  of SIGPIPE/EPIPE at once; it does exit on stdin EOF, but only once its
  turn ends (seen 2026-10-17: a crashed run's subagent wrote two more steps
  over 50 seconds before it exited). Stage 1's start already stops such an
  orphan (`stopOrphan`, by its recorded pid, if its command line still
  names its session); Stage 2's fallback path must keep doing that.
- Code signing on macOS. Rewriting the running binary in place can get the
  process killed (unverified for Go's ad-hoc signed binaries). Stage 1
  renames the build over the binary, which avoids it; keep that.
- Race detector: adoption touches everything under `a.mu`. All of it must
  happen before the hub, MCP and web serve requests.

## Tests to write first (red)

On the fake `claude` (the test binary):

1. A subagent mid-tool (`wait-for-message`) survives a reload: same pid
   after, and a tell after the reload reaches it and gets its answer.
2. A subagent at rest survives: a tell after the reload sets it to work in
   the same process (no `--resume` call).
3. Leftover bytes: the fake writes a line in two halves around the
   handoff; the new process parses it once.
4. A loop mid-turn keeps `turns` and its held tells; a hook mid-run is run
   again after.
5. A question waiting on the user is shown again after, and its answer
   reaches the fake.
6. MCP: a subagent's MCP call right after the reload succeeds (listener fd
   passed).
7. A failed exec: the old process adopts its own handoff, or resumes as in
   Stage 1; nothing is lost or doubled.
8. A crash between handoff and adoption: the next start resumes from the
   records (Stage 1) and stops the orphans.
9. A round-trip check that every field of the snapshot types is handed
   over or listed as dropped.
10. `go test -race` over all of it, and a real run on macOS and Linux with
   a real subagent mid-`sleep`.

To test the exec itself, run the test binary as a child that execs itself
(as the PoC did) rather than exec-ing the test process.

## Rejected alternatives

- **air (or watchexec, reflex, gow) alone.** These kill and restart the
  process: SIGKILL by default, SIGINT with `send_interrupt`, and on Linux
  air signals the whole process tree. Every subagent is cut either way.
  Their file watching is harmful here: subagents edit opt-chat's own
  source, so each save would restart the harness. At most a trigger, and
  `/reload` is already one.
- **tableflip / overseer-style handoff to a new process.** tableflip forks
  a new process and passes listener fds; the old one then exits. The
  `claude` children would be orphaned to launchd or init: the new process
  couldn't `wait` on them (no exit codes, zombies reaped by init), and the
  terminal's job would end while an unrelated pid ran on, so Ctrl-C no
  longer reaches it. overseer keeps a master that restarts a worker, so the
  children die with the worker unless detached. facebookgo/grace is
  archived.
- **Detached subagents** (setsid, stdout to a file, stdin from a fifo
  opened O_RDWR, or a small holder process per agent with a unix socket).
  Survives even crashes and Ctrl-C, but: quitting no longer stops agents
  (needs its own kill path and orphan sweeping, with pid reuse checks), no
  `wait` or exit codes, the MCP port and secret must be fixed, MCP is down
  while opt-chat is, events must be replayed from an offset without
  doubling log lines, and it still needs the same state snapshot as this
  design. About 600 to 900 lines, and it changes what quitting means.

## Open questions

- Is Stage 2 needed at all? Measure first: how often reloads cut a long
  tool call, and what the resumes' cache writes cost, over a few weeks of
  Stage 1.
- Pass the web listener's fd too, or keep `web.json`? Only matters if a
  page's request during the restart window is a problem.
- Should the handoff keep the control call (Claude Code's lists) alive? It
  is restarted on demand in seconds; likely not.
