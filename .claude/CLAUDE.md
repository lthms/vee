# CLAUDE.md

Vee is a session orchestrator for Claude Code with behavioral profiles.

## Development

### Commands

```bash
go build ./...
go test ./...
go vet ./...
gofmt -w .
```

### CI

All PRs must pass: build, test, vet, gofmt. Coverage tracked via Codecov.

### Code Style

- `gofmt` enforced by CI
- Tests in `*_test.go` alongside code
- No TODO/FIXME in committed code — create issues instead

## Layout

- `cmd/vee/` — CLI and daemon
- `internal/` — KB and feedback storage
- `profiles/` — Profile definitions (YAML frontmatter + markdown)
- `plugins/vee/` — Claude Code plugin
