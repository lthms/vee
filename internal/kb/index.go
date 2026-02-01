package kb

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// IndexNote performs background semantic indexing of a note.
// It summarizes the note, picks tags, finds related notes, updates the vault
// file, and inserts the note into the tree index.
func (kb *KnowledgeBase) IndexNote(noteID int) error {
	note, err := kb.getNote(noteID)
	if err != nil {
		return fmt.Errorf("get note: %w", err)
	}

	rawContent, err := kb.FetchNote(note.path)
	if err != nil {
		return fmt.Errorf("read note content: %w", err)
	}
	content := stripFrontmatter(rawContent)

	// Step 1: Summarize
	summary, err := kb.model.Generate(fmt.Sprintf(
		"Summarize the following note in one concise sentence.\n\nTitle: %s\n\nContent:\n%s\n\nReply with ONLY the summary sentence, nothing else.",
		note.title, content,
	))
	if err != nil {
		return fmt.Errorf("summarize: %w", err)
	}
	summary = strings.TrimSpace(summary)

	if _, err := kb.db.Exec(`UPDATE notes SET summary = ? WHERE id = ?`, summary, noteID); err != nil {
		return fmt.Errorf("update summary: %w", err)
	}

	// Step 2: Pick tags
	rootLabels, err := kb.allRootLabels()
	if err != nil {
		return fmt.Errorf("get root labels: %w", err)
	}

	var existingSection string
	if len(rootLabels) > 0 {
		existingSection = fmt.Sprintf("Existing categories in the knowledge base:\n%s\n\n", strings.Join(rootLabels, ", "))
	}

	tagsRaw, err := kb.model.Generate(fmt.Sprintf(
		`Pick categories/tags for the following note. Prefer reusing existing categories when they fit. Only create a new category if none of the existing ones apply. Keep categories as single lowercase words or short hyphenated phrases.

%sNote summary: %s

Title: %s

Reply with ONLY a comma-separated list of tags, nothing else.`,
		existingSection, summary, note.title,
	))
	if err != nil {
		return fmt.Errorf("tag picking: %w", err)
	}

	var tags []string
	for _, t := range strings.Split(tagsRaw, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			tags = append(tags, t)
		}
	}

	// Step 3: Find related notes
	recentNotes, err := kb.recentNoteSummaries(noteID, 20)
	if err != nil {
		slog.Warn("index: failed to get recent notes for linking", "error", err)
	}

	var relatedTitles []string
	if len(recentNotes) > 0 {
		var notesList strings.Builder
		for _, rn := range recentNotes {
			notesList.WriteString(fmt.Sprintf("- %s: %s\n", rn.title, rn.summary))
		}

		relatedRaw, err := kb.model.Generate(fmt.Sprintf(
			`Given this note and a list of other notes, identify which notes are related.

Note: %s — %s

Other notes:
%s
Reply with ONLY a comma-separated list of related note titles (exactly as shown above), or "none" if no notes are related. Nothing else.`,
			note.title, summary, notesList.String(),
		))
		if err != nil {
			slog.Warn("index: find related failed", "error", err)
		} else {
			relatedRaw = strings.TrimSpace(relatedRaw)
			if relatedRaw != "none" && relatedRaw != "" {
				for _, t := range strings.Split(relatedRaw, ",") {
					t = strings.TrimSpace(t)
					if t != "" {
						relatedTitles = append(relatedTitles, t)
					}
				}
			}
		}
	}

	// Step 4: Update vault file with tags and related links
	kb.rewriteVaultFile(note, content, tags, relatedTitles)

	// Update tags in DB
	tagStr := strings.Join(tags, ",")
	if _, err := kb.db.Exec(`UPDATE notes SET tags = ? WHERE id = ?`, tagStr, noteID); err != nil {
		slog.Error("index: update tags failed", "noteID", noteID, "error", err)
	}

	// Step 5: Tree insertion (fan out per tag)
	var wg sync.WaitGroup
	for _, tag := range tags {
		wg.Add(1)
		go func(tag string) {
			defer wg.Done()
			if err := kb.insertIntoTree(noteID, tag, summary); err != nil {
				slog.Error("index: tree insertion failed", "noteID", noteID, "tag", tag, "error", err)
			}
		}(tag)
	}
	wg.Wait()

	// Step 6: Mark as indexed
	if _, err := kb.db.Exec(`UPDATE notes SET indexed = 1 WHERE id = ?`, noteID); err != nil {
		return fmt.Errorf("mark indexed: %w", err)
	}

	slog.Info("index: note indexed", "noteID", noteID, "title", note.title, "tags", tags)
	return nil
}

// BackfillSummaries updates summaries for all leaf nodes whose summary still
// equals their label (i.e., never been updated), then propagates up to ancestors.
func (kb *KnowledgeBase) BackfillSummaries() {
	rows, err := kb.db.Query(`SELECT id FROM tree_nodes WHERE is_leaf = 1 AND summary = label`)
	if err != nil {
		slog.Error("backfill: query stale leaves", "error", err)
		return
	}
	defer rows.Close()

	var leafIDs []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			slog.Error("backfill: scan leaf id", "error", err)
			continue
		}
		leafIDs = append(leafIDs, id)
	}
	if err := rows.Err(); err != nil {
		slog.Error("backfill: iterate leaves", "error", err)
		return
	}

	if len(leafIDs) == 0 {
		return
	}

	slog.Info("backfill: updating stale leaf summaries", "count", len(leafIDs))

	for _, id := range leafIDs {
		if err := kb.updateLeafSummary(id); err != nil {
			slog.Warn("backfill: update leaf summary failed", "nodeID", id, "error", err)
			continue
		}
		if err := kb.propagateSummaryUp(id); err != nil {
			slog.Warn("backfill: propagate summary failed", "nodeID", id, "error", err)
		}
	}

	slog.Info("backfill: done")
}
