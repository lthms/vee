---
name: index
description: Required skill to index a new note into the hierarchical index tree
user-invocable: false
---

This skill receives arguments wrapped in XML tags:

- `<kb-root>`: absolute path to the knowledge base
- `<note-path>`: path to the newly created note (relative to `<kb-root>`)
- `<note-content>`: the full content of the note

The knowledge base has two sibling directories:
- `vault/`: Obsidian vault containing the notes
- `_index/`: hierarchical summaries

An `_index.md` file lives under `_index/` and is either:
- A **branch**: contains only links to sub-category `_index.md` files
  (sub-directories) with summaries
- A **leaf**: contains only links to notes (files in `vault/`) with
  one-line summaries

## Procedure

We start with `INDEXES` being the singleton list `["_index/_index.md"]` and
`VISITED` being the empty set `{}`.

1. Remove from `INDEXES` any path already in `VISITED`.
   If `INDEXES` is now empty, indexing is complete — stop.

2. Add every remaining element of `INDEXES` to `VISITED`.

3. For each element `INDEX_PATH` of `INDEXES`, spawn an `index` subagent
   (Task tool). The prompt must use XML tags — do not repeat instructions,
   the agent definition already contains them:
   ```
   <kb-root>KB_ROOT</kb-root>
   <index-path>INDEX_PATH</index-path>
   <note-path>NOTE_PATH</note-path>
   <note-content>
   NOTE_CONTENT
   </note-content>
   ```

   When multiple `INDEX_PATH` entries target **different subtrees** (no shared
   `_index.md` files), spawn them **in parallel**. This is safe because they
   write to disjoint files.

4. The subagent returns a JSON object:
   ```
   {"traverse": [...]}
   ```
   - `"traverse"`: sub-category index paths (relative to KB_ROOT) where the
     note should also be indexed.

5. Collect all `"traverse"` paths from the current level.

6. Set `INDEXES` to the collected paths and go back to step 1.
   (Step 1 will prune any paths already in `VISITED`, preventing cycles.)

## Important constraints

- All semantic decisions (relevance, sub-category creation, leaf splits)
  happen inside the `index` subagent. The skill is a mechanical router — it
  never reads or writes index files itself.
- The subagent handles leaf updates and splits directly (it has Write access).
  The skill only needs to route the tree traversal.
- A note can be indexed into multiple branches. The subagent returns all
  matching sub-categories, and the skill fans out into each of them.
- The root `_index/_index.md` is always a branch whose sub-categories
  correspond to note tags. The subagent handles this automatically — the skill
  does not need special root-level logic.
