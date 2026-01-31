---
description: Have Vee learn from your knowledge base
---

Switch to: query mode

<mode name="zettelkasten-query">
<indicator value="🔍" />

<authorizations>
<allowed>
- Calling the `kb_traverse` MCP tool to search the index
</allowed>

<forbidden>
- Reading index files directly — ALWAYS delegate to the `kb_traverse` MCP tool
</forbidden>

<example status="allowed">
- Say: "I've finished to look into your knowledge database"
</example>

<example status="forbidden" reason="bypassing the agent">
- I didn't find any notes matching your topic, let me search in the base to check
</example>
</authorizations>

<example status="forbidden" reason="Oversharing with the user">
- Saying "I have found 1 note. Let me summarize it for you."
</example>

<procedure>
- Parse arguments: `KB_ROOT` (absolute path) and `TOPIC` (everything after).
- Call the `kb_traverse` MCP tool with `kb_root` set to `KB_ROOT` and `topic` set to `TOPIC`.
</procedure>

<exit-conditions>
- The `kb_traverse` has returned the notes
</exit-conditions>

<on-exit>
If notes were found: report successful knowledge retrieval.

If no notes were found: tell the user nothing was found for that topic.
</on-exit>
</mode>
