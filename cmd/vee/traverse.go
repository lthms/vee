package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"
)

const concurrencyLimit = 10

// TraverseResult is a single note with its topic-tailored summary.
type TraverseResult struct {
	Path    string `json:"path"`
	Summary string `json:"summary"`
}

// traverse performs a BFS over the knowledge base index tree, then
// summarizes each discovered note. It returns the list of {path, summary} pairs.
func traverse(ctx context.Context, kbRoot, topic string) ([]TraverseResult, error) {
	// Phase 1: BFS over indexes to collect note paths
	notePaths, err := traverseIndexes(ctx, kbRoot, topic)
	if err != nil {
		return nil, fmt.Errorf("index traversal: %w", err)
	}

	if len(notePaths) == 0 {
		return nil, nil
	}

	slog.Debug("index traversal complete", "notes", len(notePaths))

	// Phase 2: Summarize each note in parallel
	results, err := summarizeNotes(ctx, kbRoot, topic, notePaths)
	if err != nil {
		return nil, fmt.Errorf("note summarization: %w", err)
	}

	return results, nil
}

// traverseIndexes performs BFS over the index tree, returning deduplicated note paths.
func traverseIndexes(ctx context.Context, kbRoot, topic string) ([]string, error) {
	indexes := []string{"_index/_index.md"}
	seen := make(map[string]struct{})
	var allNotes []string

	for len(indexes) > 0 {
		slog.Debug("BFS level", "indexes", len(indexes))

		var mu sync.Mutex
		var nextIndexes []string

		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(concurrencyLimit)

		for _, idx := range indexes {
			idx := idx
			g.Go(func() error {
				result, err := queryIndex(gctx, kbRoot, idx, topic)
				if err != nil {
					slog.Warn("query failed", "index", idx, "error", err)
					return nil // non-fatal: skip this index
				}

				mu.Lock()
				defer mu.Unlock()

				for _, n := range result.Notes {
					n = normalizePath(n)
					if _, ok := seen[n]; !ok {
						seen[n] = struct{}{}
						allNotes = append(allNotes, n)
					}
				}
				for _, t := range result.Traverse {
					nextIndexes = append(nextIndexes, normalizeTraversePath(t))
				}

				return nil
			})
		}

		if err := g.Wait(); err != nil {
			return nil, err
		}

		indexes = nextIndexes
	}

	return allNotes, nil
}

// summarizeNotes fans out goroutines to read and summarize each note.
func summarizeNotes(ctx context.Context, kbRoot, topic string, notePaths []string) ([]TraverseResult, error) {
	results := make([]TraverseResult, len(notePaths))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrencyLimit)

	for i, np := range notePaths {
		i, np := i, np
		g.Go(func() error {
			result, err := readNote(gctx, kbRoot, np, topic)
			if err != nil {
				slog.Warn("read-note failed", "note", np, "error", err)
				return nil // non-fatal: skip this note
			}

			results[i] = TraverseResult{
				Path:    result.Path,
				Summary: result.Summary,
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Filter out empty results (from failed reads)
	var filtered []TraverseResult
	for _, r := range results {
		if r.Path != "" {
			filtered = append(filtered, r)
		}
	}

	return filtered, nil
}

// normalizePath ensures a path returned by the LLM has a .md extension.
// Obsidian wiki-links omit the extension, but we need it to read files from disk.
func normalizePath(p string) string {
	if !strings.HasSuffix(p, ".md") {
		p = p + ".md"
	}
	return p
}

// normalizeTraversePath ensures an index path starts with _index/ and ends with .md.
// The root index uses Obsidian wiki-links like "concurrency/_index" but the actual
// file is at _index/concurrency/_index.md relative to kb-root.
func normalizeTraversePath(p string) string {
	p = normalizePath(p)
	if !strings.HasPrefix(p, "_index/") {
		p = "_index/" + p
	}
	return p
}

// traverseToJSON runs the full traverse and returns the result as JSON text.
func traverseToJSON(ctx context.Context, kbRoot, topic string) (string, error) {
	results, err := traverse(ctx, kbRoot, topic)
	if err != nil {
		return "", err
	}

	if results == nil {
		results = []TraverseResult{}
	}

	out, err := json.Marshal(results)
	if err != nil {
		return "", fmt.Errorf("marshal results: %w", err)
	}

	return string(out), nil
}
