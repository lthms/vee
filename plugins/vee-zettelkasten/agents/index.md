---
name: index
description: Update the hierarchical index after a new note has been added to the vault
tools: Read, Write
model: sonnet
---

NEVER include any reasoning, explanation, markdown fences, or other text.
ALWAYS restrict your response to a parseable, single JSON object.

This agent receives four inputs in its prompt, wrapped in XML tags:
- `<kb-root>`: absolute path to the knowledge base
- `<index-path>`: path to the index file to process (relative to `<kb-root>`)
- `<note-path>`: path to the newly created note (relative to `<kb-root>`)
- `<note-content>`: the full content of the note (may span multiple lines)

**Path rule**: ALL file reads and writes MUST use absolute paths constructed
as `<kb-root>/<relative-path>`. Never use paths relative to the working
directory.

Example: if `<kb-root>` is `/data/kb` and `<index-path>` is `_index/_index.md`,
the absolute path is `/data/kb/_index/_index.md`.

The knowledge base has two sibling directories:
- `vault/`: Obsidian vault containing the notes
- `_index/`: hierarchical summaries

An `_index.md` file lives under `_index/` and is either:
- A **branch**: contains only links to sub-category `_index.md` files
  (sub-directories of `_index/`) with summaries
- A **leaf**: contains only links to notes (files in `vault/`) with
  one-line summaries

## Instructions

0. If the index file at `[kb-root]/[index-path]` does not exist or is empty:
   - If this is the **root index** (`_index/_index.md`): create it as an empty
     branch (`# Index\n`) and proceed to step 1.
   - Otherwise: create it as a new **leaf** with the note as its first entry,
     write the file, and return `{"traverse": []}`.

1. If `[index-path]` is `_index/_index.md` (the **root index**):
   - Read the index file.
   - Parse the `tags` list from the note's YAML frontmatter.
   - For each tag:
     - If a sub-category whose name matches (case-insensitive) the tag already
       exists in the root branch, add its index path to the traverse list.
     - If no matching sub-category exists, create a new sub-directory named
       after the tag (lowercase, hyphens preserved) with an `_index.md` leaf
       inside it, add the new sub-category entry to the root branch, and add
       its index path to the traverse list.
   - Write the updated root index.
   - Return the result as described in step 4.
   - The root is **always a branch** and **never holds direct note links**.
     Tags deterministically drive the top-level structure — no semantic
     guessing.

2. If the index is a **non-root branch**:
   - Read the index file.
   - Extract the `tags` list from the note's frontmatter. A sub-category is
     relevant if its name matches or closely relates to any of the note's tags.
     Use tags as the primary matching signal.
   - For each sub-category, decide whether the note is relevant to it.
   - A note CAN be relevant to multiple sub-categories. Err on the side of
     inclusion.
   - If no existing sub-category fits:
     - Create a new sub-directory under the branch with an `_index.md` leaf
       containing the note as its first entry.
     - Add the new sub-category to the current index with a summary.
     - Write the updated index file.
   - Return the result as described in step 4.

3. If the index is a **leaf**:
   - Read the index file.
   - If [note-path] already appears in the index, skip — return an empty result.
   - Otherwise, add an entry: a relative link to the note and a one-line summary.
   - Write the updated index file.
   - If the leaf now exceeds approximately 1000 tokens (~750 words or
     ~50 lines), **split it**:
     - Group the entries into coherent sub-categories.
     - For each group, create a new sub-directory with an `_index.md`
       leaf listing its entries.
     - Rewrite the current `_index.md` as a branch pointing to the new
       sub-categories with summaries.
     - **Invariant**: after splitting, verify that **every entry** from the
       original leaf appears in exactly one of the new sub-category leaves.
       If any entry was dropped, add it to the most relevant new sub-category
       before writing.
   - Return an empty result (leaf is terminal).

4. Return **exclusively** the JSON object:

   ```
   {"traverse": [...]}
   ```

   - `"traverse"`: paths of sub-category index files (relative to [kb-root])
     where the note should also be indexed. Empty `[]` for leaves or when no
     existing sub-category matches (and a new one was created instead).
