# opt-chat

One chat with Claude that never ends, after Taelin's
[UniiChat recipe](https://gist.github.com/VictorTaelin/91837951a5ce5b38f341ec1ba1df6449).
Every message goes into an append-only log, and a cheap model folds the log
into a binary tree of one-line summaries. Each turn starts fresh from a
fixed-size view of the whole chat (recent messages one line each, older ones
coarser) and zooms into any line, down to the message itself, when it needs
more.

Claude Code is the tool loop, so every turn, subagent and summary is a
`claude -p` call on your Claude subscription. opt-chat owns the memory (the
log, the tree, the view and the `zoom`, `date`, `search`, `spawn` and `tell`
tools it serves to those calls over MCP) and a local web page you chat in,
which works much like Claude Code in a terminal. The assistant is called
Opt.

## Run

You need Go 1.27 and a logged-in `claude` CLI.

```sh
go build -o opt-chat .
./opt-chat                        # chat in ~/.opt-chat, tools run in ~
./opt-chat -dir ~/chats/work -cwd ~/dev
./opt-chat -import ~/.claude      # import your past chats, then chat
```

`./opt-chat` prints the page's address and opens it in your browser. Ctrl-C
in the terminal quits and stops every call underway; `-repl` chats in the
terminal instead. Put your own instructions in `<dir>/AGENTS.md` (who you
are, how you like work done). They follow the system prompt, so edit the
file rarely: every change starts the prompt cache over.

## What the page does

- It keeps Claude Code's keys. Enter sends, Esc stops the turn, Shift+Tab
  cycles the permission mode, Alt+P picks the model, `/` lists your skills
  (mid-sentence too), `@` completes paths, and `?` lists the rest.
- A message typed while Opt works is queued and reaches Opt between its
  tool calls, or starts the next turn.
- You attach files by dropping, pasting or typing a path. Images and PDFs go
  to the model as they are, text files inline, anything else as a path.
- Alt+K records a voice message, transcribed by Whisper on Groq when
  `GROQ_API_KEY` is set. It plays in the chat; Opt reads its words.
- Your Claude Code skills, plugins and MCP servers come along, but never
  your hooks, CLAUDE.md or memory. `/skills`, `/mcp` and `/plugins` switch
  them for Opt alone.
- Subagents run in the background, in a rail beside the chat that stops
  and clears each. A stopped one resumes when Opt tells it something.
- `-import` brings in past chats from Claude Code, Codex, or a Claude or
  ChatGPT export.
- The bar shows how big the memory is, the last turn's tokens and your
  plan's usage limits.
- The page listens on 127.0.0.1 only, behind a secret key.

## Flags

| flag | default | |
|---|---|---|
| `-dir` | `~/.opt-chat` | where the chat lives |
| `-cwd` | `~` | where Opt's tools run |
| `-model`, `-effort` | `fable`, `high` | model and effort for turns |
| `-agent-model`, `-agent-effort` | `opus`, `high` | for subagents |
| `-compact-model`, `-compact-effort` | `sonnet`, `medium` | for summaries (the recipe's: `haiku`) |
| `-permission-mode` | `auto` | `manual`, `acceptEdits` or `auto` |
| `-tools` | `Bash,Read,Edit,Write,WebSearch,WebFetch` | Claude Code tools |
| `-claude-config` | on | your skills, plugins, MCP servers and settings |
| `-import` | | import past chats first; see `-import-since`, `-import-tools`, `-import-skip` in the guide |
| `-addr` | last run's port | `127.0.0.1:port` |
| `-open` | `true` | open the page in the browser |
| `-recall` | `8` | raw messages sharing your message's rare words, given to each turn |
| `-repl` | off | chat in the terminal |

## Files

The chat lives in `-dir`: append-only logs of the messages and the summary
tree, the view, attachments, and each subagent's log and what resuming it
takes (the [guide](docs/2026-10-12-guide.md#files) lists them). Back it up, and
never edit the logs: they're the chat's history.

## More

- [Guide](docs/2026-10-12-guide.md): every key, imports, attachments, your
  Claude Code setup, subagents, and who can reach the page.
- [Design notes](docs/2026-10-12-design.md): where opt-chat departs from the
  recipe and why, with what was measured.
- [CLAUDE.md](CLAUDE.md): notes for working on the code.

## Test

```sh
go test ./...          # a few minutes: some tests fsync thousands of writes
go test -short ./...   # skips those
```

The tests run against a fake `claude`: the test binary itself, speaking
stream-json.

## License

[MIT](LICENSE). The libraries in `web/vendor` keep their own licenses
([web/vendor/LICENSES](web/vendor/LICENSES)).
