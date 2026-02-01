package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/lthms/vee/internal/kb"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DaemonCmd runs the Vee daemon (MCP server + API).
type DaemonCmd struct{}

// MCP tool args

type requestSuspendArgs struct{}
type selfDropArgs struct{}

type kbRememberArgs struct {
	Title   string   `json:"title" jsonschema:"Title of the note"`
	Content string   `json:"content" jsonschema:"Body of the note (markdown)"`
	Sources []string `json:"sources" jsonschema:"required,Origins of the information (file paths, URLs, issue references, etc.)"`
}

type kbQueryArgs struct {
	Query string `json:"query" jsonschema:"Search query. Use specific, meaningful search terms (e.g. 'tmux keybindings'). Do NOT use wildcards or glob patterns."`
}

type kbFetchArgs struct {
	Path string `json:"path" jsonschema:"Relative path of the note in the vault (as returned by kb_query)"`
}

type kbTouchArgs struct {
	Path string `json:"path" jsonschema:"Relative path of the note in the vault (as returned by kb_query)"`
}

// newMCPServer creates a fresh MCP server with all tools registered.
// Called once per SSE connection so each session gets its own initialization lifecycle.
// sessionID scopes request_suspend and self_drop to a specific session.
func newMCPServer(app *App, kbase *kb.KnowledgeBase, sessionID string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "vee",
		Version: "1.0.0",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "request_suspend",
		Description: "Request that the current Vee session be suspended so it can be resumed later.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args requestSuspendArgs) (*mcp.CallToolResult, any, error) {
		slog.Debug("request_suspend called", "session", sessionID)
		sess := app.Sessions.get(sessionID)
		if sess == nil || sess.Status != "active" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: "No active session to suspend."},
				},
			}, nil, nil
		}
		if sess.Ephemeral {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: "Ephemeral sessions cannot be suspended. Use self_drop to end the session instead."},
				},
			}, nil, nil
		}
		app.Sessions.setStatus(sess.ID, "suspended")
		slog.Debug("session suspended", "session", sess.ID)
		if sess.WindowTarget != "" {
			go func() {
				// Delay so the MCP response reaches Claude before we interrupt
				time.Sleep(2 * time.Second)
				tmuxGracefulClose(sess.WindowTarget)
			}()
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "Session suspended."},
			},
		}, nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "self_drop",
		Description: "Signal that the current task is done. Call this when your work is complete to end the session.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args selfDropArgs) (*mcp.CallToolResult, any, error) {
		slog.Debug("self_drop called", "session", sessionID)
		sess := app.Sessions.get(sessionID)
		if sess == nil || sess.Status != "active" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: "No active session to drop."},
				},
			}, nil, nil
		}
		app.Sessions.setStatus(sess.ID, "completed")
		slog.Debug("session completed", "session", sess.ID)
		if sess.WindowTarget != "" {
			go func() {
				// Delay so the MCP response reaches Claude before we interrupt
				time.Sleep(2 * time.Second)
				tmuxGracefulClose(sess.WindowTarget)
			}()
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "Session ending."},
			},
		}, nil, nil
	})

	// Knowledge base tools — available to all modes
	mcp.AddTool(server, &mcp.Tool{
		Name:        "kb_remember",
		Description: "Save a note to the persistent knowledge base. Writes an Obsidian-compatible markdown file. Tags, summaries, and tree indexing are handled automatically in the background.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args kbRememberArgs) (*mcp.CallToolResult, any, error) {
		slog.Debug("kb_remember called", "title", args.Title)

		noteID, path, err := kbase.AddNote(args.Title, args.Content, args.Sources)
		if err != nil {
			return nil, nil, fmt.Errorf("kb_remember: %w", err)
		}

		// Register indexing task and launch background indexer
		app.Indexing.add(noteID, args.Title)
		go func() {
			defer app.Indexing.remove(noteID)
			kbase.IndexNote(noteID)
		}()

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: fmt.Sprintf("Note saved: %s (indexing in background)", path)},
			},
		}, nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "kb_query",
		Description: "Search the knowledge base using semantic tree traversal. Returns matching notes with summaries. Use specific search terms, not wildcards.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args kbQueryArgs) (*mcp.CallToolResult, any, error) {
		slog.Debug("kb_query called", "query", args.Query)
		results, err := kbase.Query(args.Query)
		if err != nil {
			return nil, nil, fmt.Errorf("kb_query: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: kb.QueryResultsJSON(results)},
			},
		}, nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "kb_fetch",
		Description: "Fetch the full content of a note by its path. Use paths returned by kb_query. Multiple notes can be fetched in parallel.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args kbFetchArgs) (*mcp.CallToolResult, any, error) {
		slog.Debug("kb_fetch called", "path", args.Path)
		content, err := kbase.FetchNote(args.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("kb_fetch: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: content},
			},
		}, nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "kb_touch",
		Description: "Bump the last_verified timestamp of a note to today, confirming the information is still accurate. Use paths returned by kb_query.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args kbTouchArgs) (*mcp.CallToolResult, any, error) {
		slog.Debug("kb_touch called", "path", args.Path)
		note, err := kbase.GetNoteByPath(args.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("kb_touch: note not found: %w", err)
		}

		app.Indexing.add(note.ID, note.Title)
		go func() {
			defer app.Indexing.remove(note.ID)
			kbase.TouchNote(note.ID)
		}()

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: fmt.Sprintf("Touched: %s (last_verified updated to today)", note.Title)},
			},
		}, nil, nil
	})

	return server
}

// setupHTTPMux creates an http.ServeMux with all routes registered.
func setupHTTPMux(app *App, kbase *kb.KnowledgeBase) *http.ServeMux {
	sseHandler := mcp.NewSSEHandler(func(r *http.Request) *mcp.Server {
		sessionID := r.URL.Query().Get("session")
		return newMCPServer(app, kbase, sessionID)
	}, nil)

	mux := http.NewServeMux()
	mux.Handle("/sse", sseHandler)
	mux.HandleFunc("/api/state", handleState(app))
	mux.HandleFunc("/api/sessions", handleSessions(app))
	mux.HandleFunc("/api/config", handleConfig(app))
	mux.HandleFunc("/api/suspend", handleSuspend(app))
	mux.HandleFunc("/api/complete", handleComplete(app))
	mux.HandleFunc("/api/activate", handleActivate(app))
	mux.HandleFunc("/api/preview", handlePreview(app))
	mux.HandleFunc("/api/window-state", handleWindowState(app))
	mux.HandleFunc("/api/session-ended", handleSessionEnded(app))
	mux.HandleFunc("/api/hook/preview", handleHookPreview(app))
	mux.HandleFunc("/api/hook/window-state", handleHookWindowState(app))
	mux.HandleFunc("/api/hook/kb-ingest", handleHookKBIngest(app, kbase))
	mux.HandleFunc("/api/session", handleSession(app))
	mux.HandleFunc("/api/kb/query", handleKBQuery(kbase))
	mux.HandleFunc("/api/kb/fetch", handleKBFetch(kbase))
	return mux
}

func handleState(app *App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		activeSessions := app.Sessions.active()
		suspendedSessions := app.Sessions.suspended()
		completedSessions := app.Sessions.completed()
		indexingTasks := app.Indexing.list()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"active_sessions":    activeSessions,
			"suspended_sessions": suspendedSessions,
			"completed_sessions": completedSessions,
			"indexing_tasks":     indexingTasks,
		})
	}
}

// handleSessions handles POST /api/sessions to register a new session.
func handleSessions(app *App) http.HandlerFunc {
	type createReq struct {
		ID           string `json:"id"`
		Mode         string `json:"mode"`
		Indicator    string `json:"indicator"`
		Preview      string `json:"preview"`
		WindowTarget string `json:"window_target"`
		Ephemeral    bool   `json:"ephemeral"`
		KBIngest     bool   `json:"kb_ingest"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req createReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}

		app.Sessions.create(req.ID, req.Mode, req.Indicator, req.Preview, req.WindowTarget, req.Ephemeral, req.KBIngest)
		slog.Debug("session registered via API", "id", req.ID, "mode", req.Mode, "window", req.WindowTarget, "ephemeral", req.Ephemeral, "kb_ingest", req.KBIngest)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"status": "created"})
	}
}

// handleConfig handles GET /api/config to return the stored AppConfig.
func handleConfig(app *App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		cfg := app.Config()
		if cfg == nil {
			http.Error(w, "config not set", http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cfg)
	}
}

// handleSuspend handles POST /api/suspend to suspend a session by its tmux window target.
func handleSuspend(app *App) http.HandlerFunc {
	type suspendReq struct {
		WindowTarget string `json:"window_target"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req suspendReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}

		sess := app.Sessions.findByWindowTarget(req.WindowTarget)
		if sess == nil || sess.Status != "active" {
			http.Error(w, "no active session for this window", http.StatusNotFound)
			return
		}

		// Ephemeral sessions cannot be suspended — mark completed instead
		if sess.Ephemeral {
			app.Sessions.setStatus(sess.ID, "completed")
			slog.Debug("ephemeral session completed via suspend API", "id", sess.ID, "window", req.WindowTarget)
			if req.WindowTarget != "" {
				go tmuxGracefulClose(req.WindowTarget)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "completed", "session_id": sess.ID})
			return
		}

		app.Sessions.setStatus(sess.ID, "suspended")
		slog.Debug("session suspended via API", "id", sess.ID, "window", req.WindowTarget)

		if req.WindowTarget != "" {
			go tmuxGracefulClose(req.WindowTarget)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "suspended", "session_id": sess.ID})
	}
}

// handleComplete handles POST /api/complete to mark a session as completed by its tmux window target.
func handleComplete(app *App) http.HandlerFunc {
	type completeReq struct {
		WindowTarget string `json:"window_target"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req completeReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}

		sess := app.Sessions.findByWindowTarget(req.WindowTarget)
		if sess == nil || sess.Status != "active" {
			http.Error(w, "no active session for this window", http.StatusNotFound)
			return
		}

		app.Sessions.setStatus(sess.ID, "completed")
		slog.Debug("session completed via API", "id", sess.ID, "window", req.WindowTarget)

		if req.WindowTarget != "" {
			go tmuxGracefulClose(req.WindowTarget)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "completed", "session_id": sess.ID})
	}
}

// handleActivate handles POST /api/activate to reactivate a suspended session with a new window.
func handleActivate(app *App) http.HandlerFunc {
	type activateReq struct {
		SessionID    string `json:"session_id"`
		WindowTarget string `json:"window_target"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req activateReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}

		sess := app.Sessions.get(req.SessionID)
		if sess == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}

		app.Sessions.setStatus(req.SessionID, "active")
		app.Sessions.setWindowTarget(req.SessionID, req.WindowTarget)
		slog.Debug("session activated via API", "id", req.SessionID, "window", req.WindowTarget)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "active"})
	}
}

// handlePreview handles POST /api/preview to update a session's preview text.
func handlePreview(app *App) http.HandlerFunc {
	type previewReq struct {
		SessionID string `json:"session_id"`
		Preview   string `json:"preview"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req previewReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}

		app.Sessions.setPreview(req.SessionID, req.Preview)
		slog.Debug("preview updated", "session", req.SessionID, "preview", req.Preview)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

// handleSessionEnded handles POST /api/session-ended, called when a Claude process exits.
// If the session is still "active", marks it "completed". Leaves "suspended" sessions alone.
func handleSessionEnded(app *App) http.HandlerFunc {
	type endedReq struct {
		SessionID string `json:"session_id"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req endedReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}

		sess := app.Sessions.get(req.SessionID)
		if sess == nil {
			w.WriteHeader(http.StatusOK)
			return
		}

		if sess.Status == "active" {
			app.Sessions.setStatus(req.SessionID, "completed")
			slog.Debug("session ended (process exited)", "id", req.SessionID)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": sess.Status})
	}
}

// handleHookPreview handles POST /api/hook/preview?session=<id>.
// Accepts raw Claude hook JSON from stdin (piped via curl), extracts the prompt,
// and updates the session preview. Used by ephemeral sessions where the vee binary
// is not available inside the container.
func handleHookPreview(app *App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		sessionID := r.URL.Query().Get("session")
		if sessionID == "" {
			http.Error(w, "missing session query parameter", http.StatusBadRequest)
			return
		}

		var hookData struct {
			Prompt string `json:"prompt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&hookData); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}

		if hookData.Prompt == "" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}

		preview := hookData.Prompt
		if len(preview) > 200 {
			preview = preview[:200]
		}

		app.Sessions.setPreview(sessionID, preview)
		slog.Debug("hook preview updated", "session", sessionID, "preview", preview)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

// handleWindowState handles POST /api/window-state to update dynamic window indicators.
func handleWindowState(app *App) http.HandlerFunc {
	type windowStateReq struct {
		SessionID      string `json:"session_id"`
		Working        *bool  `json:"working,omitempty"`
		Notification   *bool  `json:"notification,omitempty"`
		PermissionMode string `json:"permission_mode,omitempty"`
		Preview        string `json:"preview,omitempty"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req windowStateReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}

		sess := app.Sessions.get(req.SessionID)
		if sess == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}

		app.Sessions.setWindowState(req.SessionID, req.Working, req.Notification, req.PermissionMode, req.Preview)

		// Re-fetch after update to get the latest state for tmux sync
		sess = app.Sessions.get(req.SessionID)
		if sess != nil {
			syncWindowOptions(sess)
		}

		slog.Debug("window-state updated", "session", req.SessionID,
			"working", req.Working, "notification", req.Notification,
			"permission_mode", req.PermissionMode)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

// handleHookWindowState handles POST /api/hook/window-state?session=<id>.
// Accepts raw Claude hook JSON from stdin (piped via curl), extracts permission_mode
// and prompt, and updates the session window state. Used by ephemeral sessions
// where the vee binary is not available inside the container.
func handleHookWindowState(app *App) http.HandlerFunc {
	type hookWindowStateReq struct {
		SessionID      string `json:"session_id"`
		Working        *bool  `json:"working,omitempty"`
		Notification   *bool  `json:"notification,omitempty"`
		PermissionMode string `json:"permission_mode,omitempty"`
		Prompt         string `json:"prompt,omitempty"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		sessionID := r.URL.Query().Get("session")
		if sessionID == "" {
			http.Error(w, "missing session query parameter", http.StatusBadRequest)
			return
		}

		var req hookWindowStateReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}

		preview := req.Prompt
		if len(preview) > 200 {
			preview = preview[:200]
		}

		app.Sessions.setWindowState(sessionID, req.Working, req.Notification, req.PermissionMode, preview)

		sess := app.Sessions.get(sessionID)
		if sess != nil {
			syncWindowOptions(sess)
		}

		slog.Debug("hook window-state updated", "session", sessionID,
			"working", req.Working, "notification", req.Notification,
			"permission_mode", req.PermissionMode)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

// handleSession handles GET /api/session?id=<id> to return a single session.
func handleSession(app *App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "missing id query parameter", http.StatusBadRequest)
			return
		}

		sess := app.Sessions.get(id)
		if sess == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sess)
	}
}

// handleHookKBIngest handles POST /api/hook/kb-ingest. Accepts task results
// from the PostToolUse hook, returns immediately, and evaluates the results
// for KB-worthy notes in the background.
func handleHookKBIngest(app *App, kbase *kb.KnowledgeBase) http.HandlerFunc {
	type ingestReq struct {
		SessionID    string `json:"session_id"`
		TaskPrompt   string `json:"task_prompt"`
		TaskResponse string `json:"task_response"`
		SubagentType string `json:"subagent_type"`
		Description  string `json:"description"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req ingestReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}

		slog.Debug("kb-ingest: received", "session", req.SessionID, "subagent_type", req.SubagentType)

		go evaluateAndIngest(kbase, app, req.SessionID, req.TaskPrompt, req.TaskResponse, req.SubagentType, req.Description)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
	}
}

// evaluateAndIngest calls the judgment model to evaluate whether a task result
// contains KB-worthy information, and if so, extracts and ingests the notes.
func evaluateAndIngest(kbase *kb.KnowledgeBase, app *App, sessionID, taskPrompt, taskResponse, subagentType, description string) {
	prompt := fmt.Sprintf(`You are evaluating whether a task result from an AI coding assistant contains information worth saving to a persistent knowledge base.

The knowledge base stores reusable facts about codebases, conventions, architecture, and patterns — things that would help future sessions. It does NOT store task-specific details, debugging logs, or ephemeral information.

Task prompt: %s
Task description: %s
Subagent type: %s

Task result:
%s

Decide whether this result contains knowledge worth persisting. If yes, extract atomic notes (each covering one concept). Each note needs a title and markdown content.

Reply with ONLY valid JSON in one of these formats:
{"ingest": false}
{"ingest": true, "notes": [{"title": "Note title", "content": "Markdown content..."}]}`,
		taskPrompt, description, subagentType, taskResponse)

	response, err := kbase.CallModel(prompt)
	if err != nil {
		slog.Warn("kb-ingest: judgment evaluation failed", "session", sessionID, "error", err)
		return
	}

	response = strings.TrimSpace(response)
	response = stripCodeFence(response)

	var result struct {
		Ingest bool `json:"ingest"`
		Notes  []struct {
			Title   string `json:"title"`
			Content string `json:"content"`
		} `json:"notes"`
	}
	if err := json.Unmarshal([]byte(response), &result); err != nil {
		slog.Warn("kb-ingest: failed to parse judgment response", "session", sessionID, "raw", response, "error", err)
		return
	}

	if !result.Ingest || len(result.Notes) == 0 {
		slog.Debug("kb-ingest: no notes to ingest", "session", sessionID)
		return
	}

	source := fmt.Sprintf("task:%s (session %s)", subagentType, sessionID)

	for _, note := range result.Notes {
		noteID, _, err := kbase.AddNote(note.Title, note.Content, []string{source})
		if err != nil {
			slog.Warn("kb-ingest: failed to add note", "title", note.Title, "error", err)
			continue
		}

		app.Indexing.add(noteID, note.Title)
		go func() {
			defer app.Indexing.remove(noteID)
			kbase.IndexNote(noteID)
		}()

		slog.Info("kb-ingest: note ingested", "session", sessionID, "title", note.Title, "noteID", noteID)
	}
}

// startHTTPServerInBackground starts the HTTP server on an OS-assigned port in a
// goroutine and returns the *http.Server and actual port for later use.
func startHTTPServerInBackground(app *App, kbase *kb.KnowledgeBase) (*http.Server, int, error) {
	mux := setupHTTPMux(app, kbase)

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, 0, fmt.Errorf("listen: %w", err)
	}

	port := ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: mux}
	go func() {
		slog.Info("http server listening", "addr", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("http server error", "error", err)
		}
	}()

	return srv, port, nil
}

// Run starts the daemon: MCP server (SSE) + API on an OS-assigned port.
func (cmd *DaemonCmd) Run() error {
	userCfg, err := loadUserConfig()
	if err != nil {
		slog.Warn("failed to load user config, using defaults", "error", err)
		userCfg = &UserConfig{
			Judgment:  JudgmentConfig{URL: "http://localhost:11434", Model: "qwen2.5:7b"},
			Knowledge: KnowledgeConfig{EmbeddingModel: "nomic-embed-text"},
		}
	}

	kbase, err := openKB(userCfg)
	if err != nil {
		return fmt.Errorf("open knowledge base: %w", err)
	}
	defer kbase.Close()

	go kbase.BackfillSummaries()
	go kbase.BackfillEmbeddings()

	app := newApp()
	mux := setupHTTPMux(app, kbase)

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	slog.Info("daemon listening", "addr", ln.Addr().String())
	return http.Serve(ln, mux)
}

// handleKBQuery handles GET /api/kb/query?q=<query>.
// Returns a JSON array of QueryResult objects.
func handleKBQuery(kbase *kb.KnowledgeBase) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		query := r.URL.Query().Get("q")
		if query == "" {
			http.Error(w, "missing q query parameter", http.StatusBadRequest)
			return
		}

		results, err := kbase.Query(query)
		if err != nil {
			http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		if results == nil {
			results = []kb.QueryResult{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(results)
	}
}

// handleKBFetch handles GET /api/kb/fetch?path=<path>.
// Returns the raw note markdown content as plain text.
func handleKBFetch(kbase *kb.KnowledgeBase) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		notePath := r.URL.Query().Get("path")
		if notePath == "" {
			http.Error(w, "missing path query parameter", http.StatusBadRequest)
			return
		}

		content, err := kbase.FetchNote(notePath)
		if err != nil {
			http.Error(w, "fetch failed: "+err.Error(), http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, content)
	}
}

// stripCodeFence extracts content from within a markdown code fence if present.
// Handles ```json ... ``` as well as prose before/after the fence.
// Returns the original string if no code fence is found.
func stripCodeFence(s string) string {
	start := strings.Index(s, "```")
	if start < 0 {
		return s
	}
	// Skip past the opening ``` and any language tag (e.g. ```json)
	inner := s[start+3:]
	if nl := strings.Index(inner, "\n"); nl >= 0 {
		inner = inner[nl+1:]
	}
	// Find closing ```
	end := strings.Index(inner, "```")
	if end < 0 {
		return s
	}
	return strings.TrimSpace(inner[:end])
}
