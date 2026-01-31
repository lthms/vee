---
name: traverse
description: Required skill to load before attempting to traverse a tree
user-invocable: false
---

This skill receives arguments wrapped in XML tags:

- `<kb-root>`: absolute path to the knowledge base
- `<topic>`: the subject to search for

The knowledge base has two sibling directories:
- `vault/`: Obsidian vault containing the notes
- `_index/`: hierarchical summaries

An `_index.md` file lives under `_index/` and is either:
- A **branch**: contains only links to sub-category `_index.md` files
  (sub-directories) with summaries
- A **leaf**: contains only links to notes (files in `vault/`) with
  one-line summaries

<example>
KB_BASE/
├── vault/
│   ├── .obsidian/
│   ├── Python GIL prevents true parallelism.md
│   ├── Rust ownership eliminates data races.md
│   └── ...
├── _index/
│   ├── _index.md
│   ├── programming/
│   │   ├── _index.md
│   │   ├── python/
│   │   │   └── _index.md
│   │   └── rust/
│   │       └── _index.md
│   └── cooking/
│       └── _index.md
</example>

## Procedure

All paths passed to subagents are **relative to KB_ROOT**.
We start with `INDEXES` being the singleton list `["_index/_index.md"]`.

1. For each element `INDEX_PATH` of `INDEXES`, spawn a `query` subagent
   (Task tool) with XML tags — do not repeat instructions, the agent
   definition already contains them:
   ```
   <kb-root>KB_ROOT</kb-root>
   <index-path>INDEX_PATH</index-path>
   <topic>TOPIC</topic>
   ```

2. The subagent returns a JSON object with two arrays:
   - `"notes"`: note paths relative to KB_ROOT (from leaf entries)
   - `"traverse"`: index file paths relative to KB_ROOT (from branch entries)

3. Collect the `"notes"` into your results.

4. If there are `"traverse"` paths, go back to step 1 with `INDEXES` set to
   the collected traverse paths (they are already relative to KB_ROOT).
     - Example: traverse returns `["_index/programming/_index.md",
       "_index/rust/_index.md"]` → use these directly as INDEXES.

5. When there are no more paths to traverse, **deduplicate** the collected
   note paths (a note may appear in multiple branches). For each unique note,
   spawn a `read-note` subagent (Task tool) with XML tags — do not repeat
   instructions:
   ```
   <kb-root>KB_ROOT</kb-root>
   <note-path>NOTE_PATH</note-path>
   <topic>TOPIC</topic>
   ```
   Spawn all `read-note` subagents **in parallel**.

6. Each `read-note` subagent returns a JSON object:
   `{"path": "...", "summary": "..."}`

7. Return the complete list of `{path, summary}` pairs.

## Important constraints

- All semantic decisions (relevance, pruning) happen inside the subagent.
  The skill is a mechanical router — it never reads index files itself.
- Subagents are cheap haiku workers. Spawn them liberally, in parallel.
