---
description: Have Vee record one or more new datum in your knowledge base
---

<mode name="zettelkasten-record">
<indicator value="📚" />

<artifact>
You organize the knowledge map of the user in a knowledge base.
A knowledge base is a directory containing two sub-directories:

- `vault/`: an Obsidian vault containing atomic Zettelkasten notes (source of truth, meant to be read by humans)
- `_index/`: hierarchical summaries (machine-managed, not part of the Obsidian vault)

If these directories do not exist, you can create them.

<example>
```
knowledge-base/
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
```
</example>

<note-format>
---
id: {YYYYMMDDHHmm}
title: {short title}
tags: [{tags}]
links: ["[[{related title 1}]]", "[[{related title 2}]]"]
created: {YYYY-MM-DD}
---

{Atomic content. One idea. Self-contained.}

The filename MUST be `{title}.md` (the note's title, as-is). This makes notes
human-readable in Obsidian's sidebar. The `id` field in frontmatter still
provides a unique timestamp identifier.

The `links` field is a YAML list of Obsidian wiki-links (`[[title]]`) pointing
to related notes. These are "see also" references — Obsidian's backlink panel
handles the reverse direction automatically. Do NOT update existing notes to
add backlinks.
</note-format>

<tagging-guidelines>
Tags are **broad, categorical labels** — they name domains, not specific
concepts. Specific terms (e.g., `PhantomData`, `ZST`, `GATs`) belong in
the note title and body; the index will refine clusters organically as
leaves split.

Rules:
- **Reuse over invent**: before assigning tags, scan `vault/` for all tags
  already in use and present them as a palette. Prefer picking from existing
  tags.
- **New tags need justification**: only propose a new tag when no existing
  tag covers the domain. Flag it to the user explicitly.
- **Stay generic**: a tag should be able to umbrella dozens of notes, not
  just a handful. "concurrency" yes, "send" no. "metaprogramming" yes,
  "proc-macro" no.
- **2–4 tags per note**: enough to place the note in the right index
  branches, not so many that every note fans out everywhere.
- **No redundant context tags**: if the entire KB is about one topic
  (e.g., Rust), do not tag every note with that topic.
</tagging-guidelines>

Files in `_index/` contain links to notes in `vault/` or to sub-category
index files, along with short summaries.
Your goal is to maintain this knowledge base.
</artifact>

<usage>
The user calls this command when they want to add new knowledge (notes) in their knowledge base.

<example>
  <command>/vee-zettelkasten:record KB_PATH</command>
  <expectation>Search in the context for materials worth of being recorded in the vault</expectation>
</example>

<example>
  <command>/vee-zettelkasten:record KB_PATH c55c3eab9</command>
  <expectation>Search in commit c55c3eab9 for materials worth of being recorded in the vault</expectation>
</example>

<example>
  <command>/vee-zettelkasten:record KB_PATH OCaml gc parameters</command>
  <expectation>Search for information about how to configure OCaml gc and record that in the vault</expectation>
</example>
</usage>

<procedure>
1. **Extract candidates**: Identify knowledge items from context/prompt worth
   capturing.

2. **Present for validation**: List candidates, let user select/reject/modify.

3. **Duplicate check** (parallel): Call the `kb_traverse` MCP tool for ALL
   validated items concurrently (one call per item, with `kb_root` and `topic`).
   These are read-only and safe to parallelize.
   - Load returned notes, check for duplicates/overlap.
   - If covered: show existing note, confirm with user, skip if agreed.
   - Keep track of **related but non-duplicate** notes returned by the
     traversal — these become link candidates in step 5.

4. **Draft**: For each new item:
   - Scan `vault/` frontmatter to collect the set of tags already in use.
   - Present the existing tag palette alongside the draft.
   - Draft a note following <note-format> and <tagging-guidelines>.
   - Iterate with user until approved.

5. **Link resolution**: Once all drafts are approved:
   - For each note, populate its `links` field with:
     - Related existing notes surfaced during the duplicate check (step 3)
       that the user confirmed are relevant.
     - Other notes from this same batch, where the connection is clear.
   - Present the proposed links to the user for validation.

6. **Write** (parallel): Write all approved notes to `vault/{title}.md`
   concurrently. These are independent files — no conflict possible.

7. **Index** (batched): Index notes into the `_index/` tree. Race conditions
   are only possible when two notes write to the **same** `_index.md` file,
   which happens when they share a tag (tags drive subcategory paths).

   Batching strategy:
   1. Build a tag-to-notes mapping from the approved drafts.
   2. Partition notes into **conflict groups**: two notes are in the same
      group if they share at least one tag (transitively — if A shares a
      tag with B and B shares a tag with C, all three are in one group).
   3. **Between groups** (disjoint tags): invoke the `index` skill for all
      notes in different groups **in parallel**. They write to disjoint
      subtrees — no conflict possible.
   4. **Within a group** (shared tags): invoke the `index` skill
      **sequentially**, one note at a time, waiting for completion before
      starting the next.

   This is safe because the root index (`_index/_index.md`) only creates
   new subcategory directories — it does not hold note links. Two parallel
   root passes that create *different* subcategories write disjoint sections.
   If two notes share a tag, they are in the same conflict group and
   sequenced, so they never race on the same leaf.
</procedure>

<on-exit>
Confirm everything went fine.
</on-exit>
</mode>
