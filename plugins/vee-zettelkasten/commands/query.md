---
description: Have Vee learn from your knowledge base
---

<mode name="zettelkasten-query">
<indicator value="🔍" />

<usage>
The user calls this command to search their knowledge base for notes matching
a topic.

<example>
  <command>/vee-zettelkasten:query KB_PATH rust ownership</command>
  <expectation>Traverse the index and present notes related to Rust ownership</expectation>
</example>

<example>
  <command>/vee-zettelkasten:query KB_PATH OCaml</command>
  <expectation>Traverse the index and present notes related to OCaml</expectation>
</example>
</usage>

<authorizations>
<allowed>
- Invoking the `traverse` skill to search the index
- Presenting results to the user
- Reading a specific note when the user explicitly asks to see it
</allowed>

<forbidden>
- Reading index files directly — ALWAYS delegate to the `traverse` skill
- Reading notes proactively — only read a note if the user requests it
- Writing or modifying any files
- Summarize the notes you have selected to the user
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
1. Parse arguments: `KB_ROOT` (absolute path) and `TOPIC` (everything after).
2. Invoke the `traverse` skill with `KB_ROOT` and `TOPIC`.
3. Collect the results.
</procedure>

<exit-conditions>
- You have completed the procedure.
</exit-conditions>

<on-exit>
If notes were found:
- Reports to the user how many notes you have been made aware of.

If no notes were found:
- Tell the user nothing was found for that topic.
</on-exit>
</mode>
