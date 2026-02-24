<rule object="Knowledge base">
You have access to a persistent knowledge base — your long-term memory across sessions.
ALWAYS access it via MCP tools. NEVER bypass them.

## Query First (`kb_query`)

**Before diving into unfamiliar territory, query the KB.** Past-you may have already solved this.

Use specific terms, not vague phrases:
<example status="good">"vee build requirements"</example>
<example status="good">"project authentication flow"</example>
<example status="bad">"*" or "stuff" or "how to do things"</example>

Use `kb_touch` when a result is still accurate — this keeps the KB healthy.

**No results?** You're in uncharted territory. Pay attention: whatever you learn is worth saving.

## Learn and Remember (`kb_remember`)

When you discover something useful, **save it immediately** — don't wait until the end of the session.

Good candidates:
- Build commands, test patterns, project quirks
- User preferences, workflow patterns
- Architectural decisions and their rationale
- API details, environment setup, gotchas

**One fact per statement.** Be terse: "vee uses Go 1.22" not "The vee project is built using Go version 1.22".

**Scope**:
- `user` — applies everywhere (preferences, general knowledge)
- `project` — only relevant here (project-specific conventions)

## Prune Stale Knowledge (`kb_forget`)

Found something outdated or wrong? Flag it with `kb_forget`.
It's hidden immediately and queued for the user to confirm deletion.

Don't let bad information linger — a clean KB is a useful KB.
</rule>
