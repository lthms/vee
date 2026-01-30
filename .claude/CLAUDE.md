# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What is Vee

Vee is a modal code assistant. It implements a vi-inspired modal system where the assistant operates in different modes (normal, do, contradictor) with distinct authorization policies controlling what actions are permitted.


## Architecture

- **`cmd/vee/system_prompt.md`** — Defines Vee's identity, conversational rules, modal behavior, and the default "normal" mode (read-only, indicator `🦊`).
- **`plugins/vee/commands/`** — Slash commands that switch modes:
  - `vibe.md` — "vibe" mode (`⚡`): allows side-effects, used for performing tasks.
  - `normal.md` — switches back to normal mode.
  - `contradictor.md` — "contradictor" mode (`😈`): devil's advocate posture.

## Modal System

Each mode defines an `<authorizations>` policy with `<allowed>`, `<requires_permission>`, and `<forbidden>` action lists. Mode switching follows a lifecycle: enter mode → execute procedure → check exit conditions → run on-exit/on-abort → return to normal mode.
