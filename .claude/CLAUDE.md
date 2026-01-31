# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What is Vee

Vee is a modal code assistant. It implements a vi-inspired modal system where the Go binary orchestrates sessions — each mode gets a fresh Claude Code session with a composed system prompt (base + mode-specific).

## Architecture

The Go binary (`cmd/vee`) is the mode orchestrator. It runs a TUI loop and spawns a fresh Claude Code session for each mode invocation. Each session gets a composed system prompt (base + mode-specific) and runs independently.

- **`cmd/vee/prompts/base.md`** — Shared identity and conversational rules (embedded via `go:embed`).
- **`cmd/vee/prompts/*.md`** — Per-mode prompt definitions:
  - `normal.md` — read-only mode (`🦊`)
  - `vibe.md` — task execution mode (`⚡`)
  - `contradictor.md` — devil's advocate (`😈`)
  - `zettelkasten_query.md` — knowledge base query (`🔍`)
  - `zettelkasten_record.md` — knowledge base recording (`📚`)
- **`cmd/vee/daemon.go`** — MCP server + dashboard + HTTP API for mode tracking.
- **`cmd/vee/main.go`** — TUI loop, mode registry, session launcher.

## Modal System

The Go binary enforces the modal system: it controls which plugins are loaded, which MCP tools are available, and which system prompt is composed per session. Mode prompts define personality and purpose, not access control.

# Instructions

When the user highlights a breach in a mode policy, NEVER apologies.
ALWAYS look for what may have prompted the mismatch in your context and suggest patches to the affected command.
