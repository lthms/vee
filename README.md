# Vee

Vee is a session orchestrator for Claude Code. It runs short, disposable
sessions inside a tmux multiplexer — each scoped to a task, each with the right
behavioral profile. What matters persists in issues, PRs, and a shared
knowledge base. The conversation itself is throwaway.

Press `Ctrl-b c`, pick a profile, and a new Claude session spins up in its own
tmux window. Run several in parallel. Suspend one, resume it later. When a
session is done, it drops itself.

## Ephemeral containers

By default, sessions run directly on your host. Enable ephemeral mode and each
session gets its own Docker container instead.

The assistant can install packages, modify files, run tests — without stepping
on your local environment or on another session's work. You can have three
tasks running in parallel without juggling worktrees or stashing changes. Each
one gets its own fresh copy of the project, its own service stack, its own mess
to make. When a session ends, the container goes with it.

Add a Compose file and the assistant gets the full stack — databases, caches,
message brokers — accessible by service name.

```ini
# .vee/config
[ephemeral]
  dockerfile = Dockerfile
  compose = docker-compose.yml
  env = DATABASE_URL=postgres://postgres:postgres@db:5432/app
```

GPG commit signing works transparently — the container forwards signing
requests to your host's GPG agent, so commits are signed without exporting
keys.

## Multiplexer

Vee runs inside tmux. Each project gets its own server. The first window is a
dashboard showing active, suspended, and completed sessions. Every Claude
session gets its own window.

Detach with `Ctrl-b d` and the daemon keeps running. Rerun `vee start` to
reattach.


| Key | Action |
|-----|--------|
| `Ctrl-b c` | New session (profile picker) |
| `Ctrl-b q` | Suspend current session |
| `Ctrl-b r` | Resume a suspended session |
| `Ctrl-b k` | Kill current session |
| `Ctrl-b /` | Knowledge base explorer |
| `Ctrl-b p` | View system prompt |
| `Ctrl-b l` | View logs |
| `Ctrl-b x` | Shutdown (suspend all, exit) |
| `Ctrl-b d` | Detach (daemon stays alive) |


## Feedback loop

When the assistant does something right or wrong, record it with `/feedback`.
Examples are scoped per-project or globally, and injected into future system
prompts for that profile.

## Configuration

Git-config format with `[include]` and `[includeIf "gitdir:..."]` support.

**User config** (`~/.config/vee/config`) — embedding backend, identity,
feedback settings.

**Project config** (`.vee/config`) — forge URLs, ephemeral setup, per-project
identity.

**Project prompt** (`.vee/config.md`) — Markdown injected into every session's
system prompt.
