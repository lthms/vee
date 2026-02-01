# Vee

Vee runs disposable Claude Code sessions that communicate through your existing
tools—issues, PRs, and a persistent knowledge base—not through conversation
history.

Each session is scoped to a task: investigate a bug, plan an implementation,
execute the work. When it's done, it drops itself. What matters for the team
lives in issues and PRs. What matters for Vee's long-term effectiveness lives in
its knowledge base. The conversation is disposable.

## Why not one long conversation?

Long conversations degrade. The model forgets instructions buried 200 messages
ago, behavioral constraints drift, and context gets polluted. Vee sidesteps this
by keeping sessions short and stateless. The system prompt stays lean — just the
behavioral profile. Accumulated project knowledge is retrieved on-demand via MCP,
not crammed into every context window.

## Modes

Sessions are launched with a behavioral profile:

- 🦊 **normal** — read-only exploration, no side-effects
- ⚡ **vibe** — task execution, makes autonomous decisions
- 😈 **contradictor** — devil's advocate, challenges your position
- 🤖 **claude** — vanilla Claude Code

## Usage

```
vee start --vee-path ~/path/to/vee [-- <claude args>]
```

Arguments after `--` are forwarded to every `claude` invocation.

## Keybindings

| Key | Action |
|-----|--------|
| `Ctrl-b c` | New session (mode picker) |
| `Ctrl-b x` | Suspend session |
| `Ctrl-b r` | Resume session |
| `Ctrl-b k` | End session |
| `Ctrl-b q` | Shutdown (suspends all, exits) |
| `Ctrl-b l` | View logs |

## Project configuration

Drop a `.vee/config.md` in your project root to inject context into every
session's system prompt.
