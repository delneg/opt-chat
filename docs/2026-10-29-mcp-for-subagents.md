# MCP servers for subagents only

Each of the user's MCP servers in `/mcp` is now on, for subagents only, or
off; Enter or Space goes through the three. A server costs Opt's prompt its
tool names (with ToolSearch, the schemas load only when used) and its
instructions, every turn, and Opt can call it on its own. For subagents
only, it is off in Opt's calls and side questions, and on in the calls of
the subagents Opt gives it to.

- `settings.json` keeps the choice (`mcpAgents`, beside `mcpOff`). A server
  never set stays on, as before.
- Each call is given the servers kept out of it in its own settings,
  `deniedMcpServers`: those turned off, and those for subagents only that
  it was not given (`mcpOffLocked`). The page's own call (the `/mcp`
  panel's) is given none, so the panel shows every server's real status.
- Until now a server turned off was switched off in each call by Claude
  Code's `mcp_toggle`. Claude Code keeps that switch in the user's own
  config for the call's folder (`~/.claude.json`, `disabledMcpServers`),
  so the server stayed off in every later call there, the user's own
  sessions included, and a subagent given it started without it (seen
  live). `deniedMcpServers` holds for the one call and writes nothing
  (measured: a plugin's server, a user's and a claude.ai connector).
- `spawn` and `tell` take `mcp` only while there are servers for subagents
  only, and their description names them, sorted, so Opt's cached prompt is
  the same bytes from turn to turn. Changing a server's state rewrites it
  once.
- A subagent's call waits for its given servers as for the others, then
  warns the user about any not connected.
- The subagent's record keeps its servers (`mcp`), so a resume has them.
  Setting a server on and back keeps them; turning it off takes it from all
  while it is off.
- A denied server can't come into a call underway. `tell` with `mcp`
  restarts the subagent's call from its saved session (`restartLocked`)
  when the call lacks one of the servers: one at rest at once, one at work
  at its next step, interrupted there as the page's compact does; the tells
  sent meanwhile come with it. One compacting restarts once compacted; one
  in a loop gets the server with its next call. A tell refused (to one
  starting) grants nothing, and Opt tells it again.
- `r` reconnects a server in the page's call and in every call underway
  that has it.
- Only Opt gives servers. A subagent that needs one says so in its report,
  and Opt can give it with `tell`.
