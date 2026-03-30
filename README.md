# Vee

Vee is a lightweight wrapper around Claude Code that adds behavioral profiles,
a persistent knowledge base, and optional ephemeral containers.

Pick a profile, and Vee composes the right system prompt, starts an MCP sidecar
for the knowledge base and feedback tools, then exec's into Claude. No daemon,
no multiplexer — just a thin layer on top of Claude Code.

## Profiles

Profiles are Markdown files with YAML frontmatter that shape how Claude
behaves. Vee ships with a handful (plan, implement, design, vibe, …) and you
can add your own under `~/.local/share/vee/profiles/` or `.vee/profiles/`.

```bash
vee -p implement "add retry logic to the HTTP client"
vee -p plan
```

## Knowledge base

Enable `--kb` and Vee gives Claude long-term memory via MCP tools
(`kb_remember`, `kb_query`, `kb_forget`, `kb_touch`). Facts persist across
sessions in a local SQLite database and are deduplicated automatically.

## Feedback loop

Enable `--feedback` and the assistant can record good/bad behavioral examples
with `/feedback`. Examples are scoped per-project or globally and injected into
future system prompts for that profile.

## Ephemeral containers

Pass `--ephemeral` and the session runs inside a disposable Docker container
instead of on your host.

The assistant can install packages, modify files, run tests — without touching
your local environment. Add a Compose file and it gets the full stack
(databases, caches, message brokers) accessible by service name.

```ini
# .vee/config
[ephemeral]
  dockerfile = Dockerfile
  compose = docker-compose.yml
  env = DATABASE_URL=postgres://postgres:postgres@db:5432/app
```

GPG commit signing works transparently — the container forwards signing
requests to the host's GPG agent via the MCP sidecar.

## Configuration

Git-config format with `[include]` and `[includeIf "gitdir:..."]` support.

**User config** (`~/.config/vee/config`) — embedding backend, identity,
feedback settings.

**Project config** (`.vee/config`) — forge URLs, ephemeral setup, per-project
identity.

**Project prompt** (`.vee/config.md`) — Markdown injected into every session's
system prompt.
