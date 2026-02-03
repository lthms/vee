package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lthms/vee/internal/feedback"
	"github.com/lthms/vee/internal/kb"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DaemonCmd runs the Vee daemon (MCP server + API).
type DaemonCmd struct{}

// MCP tool args

type requestSuspendArgs struct{}
type selfDropArgs struct{}

type kbRememberArgs struct {
	Content    string `json:"content" jsonschema:"The statement to save. Must be a single atomic fact (max 2000 chars)."`
	Source     string `json:"source" jsonschema:"Origin of the information (file path, URL, issue reference, etc.)"`
	SourceType string `json:"source_type,omitempty" jsonschema:"Type of source (default: manual)"`
}

type kbQueryArgs struct {
	Query string `json:"query" jsonschema:"Search query. Use specific, meaningful search terms (e.g. 'tmux keybindings'). Do NOT use wildcards or glob patterns."`
}

type kbFetchArgs struct {
	ID string `json:"id" jsonschema:"Statement ID (as returned by kb_query)"`
}

type kbTouchArgs struct {
	ID string `json:"id" jsonschema:"Statement ID (as returned by kb_query)"`
}

type feedbackRecordArgs struct {
	Kind      string `json:"kind" jsonschema:"Whether this is a good or bad example (good or bad)"`
	Statement string `json:"statement" jsonschema:"The example or counter-example statement"`
	Scope     string `json:"scope" jsonschema:"Scope: user (all projects) or project (this project only)"`
}

// newMCPServer creates a fresh MCP server with all tools registered.
// Called once per SSE connection so each session gets its own initialization lifecycle.
// sessionID scopes request_suspend and self_drop to a specific session.
func newMCPServer(app *App, kbase *kb.KnowledgeBase, fstore *feedback.Store, sessionID string) *mcp.Server {
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
		Description: "Save a statement to the persistent knowledge base. The statement is queued for async duplicate detection and will be promoted to active once processed.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args kbRememberArgs) (*mcp.CallToolResult, any, error) {
		slog.Debug("kb_remember called")

		result, err := kbase.AddStatement(args.Content, args.Source, args.SourceType)
		if err != nil {
			return nil, nil, fmt.Errorf("kb_remember: %w", err)
		}

		msg := fmt.Sprintf("Statement saved (id: %s, status: pending — will be promoted after duplicate check)", result.ID)

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: msg},
			},
		}, nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "kb_query",
		Description: "Search the knowledge base using semantic similarity. Returns matching statements with scores. Use specific search terms, not wildcards.",
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
		Description: "Fetch the full content of a statement by its ID. Use IDs returned by kb_query. Multiple statements can be fetched in parallel.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args kbFetchArgs) (*mcp.CallToolResult, any, error) {
		slog.Debug("kb_fetch called", "id", args.ID)
		content, err := kbase.FetchStatement(args.ID)
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
		Description: "Bump the last_verified timestamp of a statement to today, confirming the information is still accurate. Use IDs returned by kb_query.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args kbTouchArgs) (*mcp.CallToolResult, any, error) {
		slog.Debug("kb_touch called", "id", args.ID)
		if err := kbase.TouchStatement(args.ID); err != nil {
			return nil, nil, fmt.Errorf("kb_touch: %w", err)
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: fmt.Sprintf("Touched: %s (last_verified updated to today)", args.ID)},
			},
		}, nil, nil
	})

	// Feedback tool — record mode-specific examples
	if fstore != nil {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "feedback_record",
			Description: "Record a good or bad example of mode behavior. The mode is inferred automatically from the current session.",
		}, func(ctx context.Context, req *mcp.CallToolRequest, args feedbackRecordArgs) (*mcp.CallToolResult, any, error) {
			slog.Debug("feedback_record called", "session", sessionID)

			if args.Kind != "good" && args.Kind != "bad" {
				return &mcp.CallToolResult{
					Content: []mcp.Content{
						&mcp.TextContent{Text: "kind must be 'good' or 'bad'"},
					},
					IsError: true,
				}, nil, nil
			}
			if args.Scope != "user" && args.Scope != "project" {
				return &mcp.CallToolResult{
					Content: []mcp.Content{
						&mcp.TextContent{Text: "scope must be 'user' or 'project'"},
					},
					IsError: true,
				}, nil, nil
			}

			sess := app.Sessions.get(sessionID)
			if sess == nil {
				return &mcp.CallToolResult{
					Content: []mcp.Content{
						&mcp.TextContent{Text: "No active session found."},
					},
					IsError: true,
				}, nil, nil
			}

			project, _ := os.Getwd()

			id, err := fstore.Record(sess.Mode, args.Kind, args.Statement, args.Scope, project)
			if err != nil {
				return nil, nil, fmt.Errorf("feedback_record: %w", err)
			}

			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: fmt.Sprintf("Feedback recorded (id: %s, mode: %s, kind: %s, scope: %s)", id, sess.Mode, args.Kind, args.Scope)},
				},
			}, nil, nil
		})
	}

	return server
}

// setupHTTPMux creates an http.ServeMux with all routes registered.
func setupHTTPMux(app *App, kbase *kb.KnowledgeBase, fstore *feedback.Store) *http.ServeMux {
	sseHandler := mcp.NewSSEHandler(func(r *http.Request) *mcp.Server {
		sessionID := r.URL.Query().Get("session")
		return newMCPServer(app, kbase, fstore, sessionID)
	}, nil)

	mux := http.NewServeMux()
	mux.Handle("/sse", sseHandler)
	mux.HandleFunc("/api/state", handleState(app, kbase))
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
	mux.HandleFunc("/api/session", handleSession(app))
	mux.HandleFunc("/api/kb/query", handleKBQuery(kbase))
	mux.HandleFunc("/api/kb/fetch", handleKBFetch(kbase))
	mux.HandleFunc("/api/kb/issues", handleKBIssues(kbase))
	mux.HandleFunc("/api/kb/issues/resolve", handleKBIssueResolve(kbase))
	if fstore != nil {
		mux.HandleFunc("/api/feedback/sample", handleFeedbackSample(fstore, app))
	}
	mux.HandleFunc("/api/session/prompt", handleSessionPrompt(app))
	return mux
}

func handleState(app *App, kbase *kb.KnowledgeBase) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		activeSessions := app.Sessions.active()
		suspendedSessions := app.Sessions.suspended()
		completedSessions := app.Sessions.completed()
		indexingTasks := app.Indexing.list()

		issueCount := 0
		if n, err := kbase.OpenIssueCount(); err == nil {
			issueCount = n
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"active_sessions":    activeSessions,
			"suspended_sessions": suspendedSessions,
			"completed_sessions": completedSessions,
			"indexing_tasks":     indexingTasks,
			"issue_count":        issueCount,
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
		SystemPrompt string `json:"system_prompt"`
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

		app.Sessions.create(req.ID, req.Mode, req.Indicator, req.Preview, req.WindowTarget, req.Ephemeral, req.SystemPrompt)
		slog.Debug("session registered via API", "id", req.ID, "mode", req.Mode, "window", req.WindowTarget, "ephemeral", req.Ephemeral)

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
		if r := []rune(preview); len(r) > 200 {
			preview = string(r[:200])
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
		if r := []rune(preview); len(r) > 200 {
			preview = string(r[:200])
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

// startHTTPServerInBackground starts the HTTP server on an OS-assigned port in a
// goroutine and returns the *http.Server and actual port for later use.
func startHTTPServerInBackground(app *App, kbase *kb.KnowledgeBase, fstore *feedback.Store) (*http.Server, int, error) {
	mux := setupHTTPMux(app, kbase, fstore)

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
		userCfg = hydrateUserConfig(nil)
	}

	kbase, err := openKB(userCfg)
	if err != nil {
		return fmt.Errorf("open knowledge base: %w", err)
	}
	defer kbase.Close()

	stDir, err := stateDir()
	if err != nil {
		return fmt.Errorf("state dir: %w", err)
	}
	fstore, err := feedback.Open(filepath.Join(stDir, "feedback.db"))
	if err != nil {
		return fmt.Errorf("open feedback store: %w", err)
	}
	defer fstore.Close()

	app := newApp()
	mux := setupHTTPMux(app, kbase, fstore)

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

// handleKBFetch handles GET /api/kb/fetch?id=<id>.
// Returns the statement as JSON.
func handleKBFetch(kbase *kb.KnowledgeBase) http.HandlerFunc {
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

		s, err := kbase.GetStatement(id)
		if err != nil {
			http.Error(w, "fetch failed: "+err.Error(), http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s)
	}
}

// handleKBIssues handles GET /api/kb/issues — returns all open issues.
func handleKBIssues(kbase *kb.KnowledgeBase) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		issues, err := kbase.ListOpenIssues()
		if err != nil {
			http.Error(w, "list issues: "+err.Error(), http.StatusInternalServerError)
			return
		}

		if issues == nil {
			issues = []kb.Issue{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(issues)
	}
}

// handleKBIssueResolve handles POST /api/kb/issues/resolve?id=<id>.
func handleKBIssueResolve(kbase *kb.KnowledgeBase) http.HandlerFunc {
	type resolveReq struct {
		Action string `json:"action"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		issueID := r.URL.Query().Get("id")
		if issueID == "" {
			http.Error(w, "missing id query parameter", http.StatusBadRequest)
			return
		}

		var req resolveReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}

		if err := kbase.ResolveIssue(issueID, req.Action); err != nil {
			http.Error(w, "resolve: "+err.Error(), http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "resolved"})
	}
}

// handleSessionPrompt handles GET /api/session/prompt?window=<window_id>.
// Returns the system prompt for the session in the given window.
func handleSessionPrompt(app *App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		windowID := r.URL.Query().Get("window")
		if windowID == "" {
			http.Error(w, "missing window query parameter", http.StatusBadRequest)
			return
		}

		sess := app.Sessions.findByWindowTarget(windowID)
		if sess == nil {
			http.Error(w, "no session for this window", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"mode":          sess.Mode,
			"indicator":     sess.Indicator,
			"system_prompt": sess.SystemPrompt,
		})
	}
}

// handleFeedbackSample handles GET /api/feedback/sample?mode=<mode>&project=<project>&n=<n>.
// Returns a JSON array of sampled feedback entries.
func handleFeedbackSample(fstore *feedback.Store, app *App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		mode := r.URL.Query().Get("mode")
		if mode == "" {
			http.Error(w, "missing mode query parameter", http.StatusBadRequest)
			return
		}

		project := r.URL.Query().Get("project")

		n := 5
		if nStr := r.URL.Query().Get("n"); nStr != "" {
			if v, err := strconv.Atoi(nStr); err == nil && v > 0 {
				n = v
			}
		}

		entries, err := fstore.Sample(mode, project, n)
		if err != nil {
			http.Error(w, "sample failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		if entries == nil {
			entries = []feedback.Entry{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(entries)
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
