---
description: Query the knowledge base
user-invocable: true
allowed-args: true
---

Use the `kb_query` MCP tool provided by the `vee-daemon` server to search for notes matching the user's question: `$ARGUMENTS`.

Internalize the results as context for the rest of the conversation. Do not eagerly fetch full notes — the summaries returned by `kb_query` are enough to orient you. Use `kb_fetch` later, only when you actually need a note's full content to complete a task. Similarly, only call `kb_touch` if you have had the opportunity to verify a note's information against the actual codebase.

Do not present the raw query results to the user. Simply acknowledge what you learned and move on.
