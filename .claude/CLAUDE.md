# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What is Vee

Vee is a modal code assistant. It implements a vi-inspired modal system where the assistant operates in different modes (normal, do, contradictor) with distinct authorization policies controlling what actions are permitted.

## Architecture

The Go binary (`cmd/vee`) is the mode orchestrator. It runs a TUI loop and spawns a fresh Claude Code session for each mode invocation. Each session gets a composed system prompt (base + mode-specific) and runs independently.

- **`cmd/vee/prompts/base.md`** — Shared identity, conversational rules, and modal framework (embedded via `go:embed`).
- **`cmd/vee/prompts/*.md`** — Per-mode prompt definitions:
  - `normal.md` — read-only mode (`🦊`)
  - `vibe.md` — side-effects allowed (`⚡`)
  - `contradictor.md` — devil's advocate (`😈`)
  - `zettelkasten_query.md` — knowledge base query (`🔍`)
  - `zettelkasten_record.md` — knowledge base recording (`📚`)
- **`cmd/vee/daemon.go`** — MCP server + dashboard + HTTP API for mode tracking.
- **`cmd/vee/main.go`** — TUI loop, mode registry, session launcher.

## Modal System

Each mode defines an `<authorizations>` policy with `<allowed>`, `<requires_permission>`, and `<forbidden>` action lists. Mode switching is handled by the Go binary: the user types a mode name (+ optional message) in the TUI, Go spawns a Claude session with the corresponding prompt, and reports the mode change to the daemon via `POST /api/mode`.

# Instructions

When the user highlights a breach in a mode policy, NEVER apologies.
ALWAYS look for what may have prompted the mismatch in your context and suggest patches to the affected command.
