# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What is Vee

Vee is a session orchestrator for Claude Code. It implements behavioral profiles where the Go binary orchestrates sessions — each profile gets a fresh Claude Code session with a composed system prompt (base + profile-specific).

## Development

### Build & Test

```bash
go build ./...          # build all packages
go test ./...           # run tests
go vet ./...            # static analysis
gofmt -w .              # format code
```

### CI Requirements

All PRs must pass:
- Build
- Tests (with coverage uploaded to Codecov)
- `go vet`
- `gofmt` check (no unformatted files)

### Code Style

- Code must be formatted with `gofmt` (enforced by CI)
- No TODO/FIXME comments in committed code — create issues instead
- Tests live alongside code in `*_test.go` files
- Internal packages go in `internal/` (not importable externally)

### Testing

- Write tests for new functionality
- Update tests when changing function signatures
- Run `go test ./...` locally before pushing
- Coverage is tracked via Codecov — avoid reducing coverage on PRs

## Architecture

The Go binary (`cmd/vee`) is the profile orchestrator. It manages a tmux-based multiplexer where each session runs in its own tmux window with a composed system prompt (base + profile-specific).

### Core Files

- **`cmd/vee/main.go`** — CLI entry point (Kong framework), subcommand dispatch, session launcher.
- **`cmd/vee/app.go`** — Shared application state (`AppConfig`) and in-memory session store.
- **`cmd/vee/daemon.go`** — MCP server (SSE-based) + HTTP API. Exposes tools (`request_suspend`, `kb_remember`, `kb_query`, `kb_touch`) and manages session state via REST endpoints.
- **`cmd/vee/tmux.go`** — Tmux integration: window creation, keybindings, graceful session shutdown.
- **`cmd/vee/dashboard.go`** — Terminal UI dashboard (Bubble Tea).
- **`cmd/vee/picker.go`** — Interactive profile picker TUI (Bubble Tea).
- **`cmd/vee/profiles.go`** — Profile loading from filesystem, prompt composition.
- **`cmd/vee/config.go`** — Configuration parser: git-config-format files with `[include]`/`[includeIf]` support.
- **`cmd/vee/ephemeral.go`** — Docker-based ephemeral session support.

### Internal Packages

- **`internal/kb/`** — Knowledge base: SQLite + embedding-based KNN search.
- **`internal/feedback/`** — Feedback storage for profile behavior examples.

### Prompts & Profiles

- **`cmd/vee/prompts/base.md`** — Shared base prompt (embedded via `go:embed`).
- **`profiles/*.md`** — Profile definitions with YAML frontmatter (indicator, description, priority) and markdown body.

Available profiles: `claude`, `contradictor`, `design`, `implement`, `issue`, `normal`, `plan`, `vibe`.

### Plugins

- **`plugins/vee/`** — Core Vee plugin providing user-invocable commands (e.g., `/suspend`, `/feedback`).

## Tmux Multiplexer

Each project gets its own tmux server via a unique socket name derived from the absolute CWD (`vee-<hash>`). The dashboard occupies the first window (running `_serve`); each Claude session gets its own window.

**Key bindings:**
- `Ctrl-b c` — New session (opens profile picker)
- `Ctrl-b q` — Suspend current session
- `Ctrl-b k` — Kill current session
- `Ctrl-b r` — Resume a suspended session
- `Ctrl-b l` — View logs
- `Ctrl-b d` — Detach (daemon stays alive)
- `Ctrl-b x` — Exit (suspend all sessions, kill tmux)

## Session Lifecycle

Sessions move through statuses: **active** → **suspended** → **completed**.

1. User picks a profile via the profile picker.
2. CLI registers the session with the daemon and spawns Claude in a new tmux window.
3. The session can be suspended (`Ctrl-b q` or MCP `request_suspend`) and later resumed (`Ctrl-b r`, using `--resume`).
4. On Claude exit, the session is marked completed.

## Knowledge Base

A shared knowledge base is available to all profiles via MCP tools (`kb_remember`, `kb_query`, `kb_touch`). Notes are stored as Obsidian-compatible markdown files with YAML frontmatter and indexed using embedding-based semantic search. Each note tracks a `last_verified` timestamp for freshness.

# Instructions

When the user highlights a breach in a profile policy, NEVER apologize. ALWAYS look for what may have prompted the mismatch in your context and suggest patches to the affected command.

Prompts use one-line paragraphs (no hard wraps). The prompt viewer handles word wrapping at display time.
