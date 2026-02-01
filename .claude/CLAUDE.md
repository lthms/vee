# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What is Vee

Vee is a modal code assistant. It implements a vi-inspired modal system where the Go binary orchestrates sessions — each mode gets a fresh Claude Code session with a composed system prompt (base + mode-specific).

## Architecture

The Go binary (`cmd/vee`) is the mode orchestrator. It manages a tmux-based multiplexer where each session runs in its own tmux window with a composed system prompt (base + mode-specific).

### Core Files

- **`cmd/vee/main.go`** — CLI entry point (Kong framework), mode registry, subcommand dispatch, session launcher.
- **`cmd/vee/app.go`** — Shared application state (`AppConfig`) and in-memory session store.
- **`cmd/vee/daemon.go`** — MCP server (SSE-based) + HTTP API. Exposes tools (`request_suspend`, `self_drop`, `kb_remember`, `kb_query`) and manages session state via REST endpoints.
- **`cmd/vee/tmux.go`** — Tmux integration: window creation, keybindings, graceful session shutdown.
- **`cmd/vee/dashboard.go`** — Terminal UI dashboard rendering active/suspended/completed sessions.
- **`cmd/vee/picker.go`** — Interactive mode picker TUI with prompt input.
- **`cmd/vee/kb.go`** — Knowledge base: SQLite FTS5 index + Obsidian-compatible markdown vault (`~/.local/state/vee/vault/`).

### Prompts

- **`cmd/vee/prompts/base.md`** — Shared identity, conversational rules, KB rules (embedded via `go:embed`).
- **`cmd/vee/prompts/normal.md`** — Read-only exploration mode (`🦊`).
- **`cmd/vee/prompts/vibe.md`** — Task execution mode (`⚡`).
- **`cmd/vee/prompts/contradictor.md`** — Devil's advocate mode (`😈`).

### Plugins

- **`plugins/vee/`** — Core Vee plugin providing user-invocable commands (e.g., `/suspend`).

## Modal System

The Go binary enforces the modal system: it controls which MCP tools are available and which system prompt is composed per session. Mode prompts define personality and purpose, not access control.

**Modes:**
- `normal` (`🦊`) — Read-only exploration
- `vibe` (`⚡`) — Task execution with side-effects
- `contradictor` (`😈`) — Devil's advocate
- `claude` (`🤖`) — Vanilla Claude Code (no system prompt injection)

## Tmux Multiplexer

Vee runs inside a tmux session (`-L vee` socket). The dashboard occupies the first window; each Claude session gets its own window.

**Key bindings:**
- `Ctrl-b c` — New session (opens mode picker)
- `Ctrl-b x` — Suspend current session
- `Ctrl-b r` — Resume a suspended session
- `Ctrl-b l` — View logs
- `Ctrl-b q` — Graceful shutdown

## Session Lifecycle

Sessions move through statuses: **active** → **suspended** → **completed**.

1. User picks a mode via the mode picker.
2. CLI registers the session with the daemon and spawns Claude in a new tmux window.
3. The session can be suspended (`Ctrl-b x` or MCP `request_suspend`) and later resumed (`Ctrl-b r`, using `--resume`).
4. On Claude exit, the session is marked completed.

## Knowledge Base

A shared knowledge base is available to all modes via MCP tools (`kb_remember`, `kb_query`, `kb_fetch`, `kb_touch`). Notes are stored as Obsidian-compatible markdown files with YAML frontmatter and indexed in a SQLite tree-based semantic index. Each note tracks a `last_verified` timestamp for freshness.

# Instructions

When the user highlights a breach in a mode policy, NEVER apologies.
ALWAYS look for what may have prompted the mismatch in your context and suggest patches to the affected command.
