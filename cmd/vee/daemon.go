package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

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
func newMCPServer(app *App, kb *KnowledgeBase) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "vee",
		Version: "1.0.0",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "request_suspend",
		Description: "Request that the current Vee session be suspended so it can be resumed later.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args requestSuspendArgs) (*mcp.CallToolResult, any, error) {
		slog.Debug("request_suspend called")
		activeSessions := app.Sessions.active()
		if len(activeSessions) > 0 {
			sess := activeSessions[0]
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
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "No active session to suspend."},
			},
		}, nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "self_drop",
		Description: "Signal that the current task is done. Call this when your work is complete to end the session.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args selfDropArgs) (*mcp.CallToolResult, any, error) {
		slog.Debug("self_drop called")
		activeSessions := app.Sessions.active()
		if len(activeSessions) > 0 {
			sess := activeSessions[0]
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
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "No active session to drop."},
			},
		}, nil, nil
	})

	// Knowledge base tools — available to all modes
	mcp.AddTool(server, &mcp.Tool{
		Name:        "kb_remember",
		Description: "Save a note to the persistent knowledge base. Writes an Obsidian-compatible markdown file. Tags, summaries, and tree indexing are handled automatically in the background.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args kbRememberArgs) (*mcp.CallToolResult, any, error) {
		slog.Debug("kb_remember called", "title", args.Title)

		noteID, path, err := kb.AddNote(args.Title, args.Content, args.Sources)
		if err != nil {
			return nil, nil, fmt.Errorf("kb_remember: %w", err)
		}

		// Register indexing task and launch background indexer
		app.Indexing.add(noteID, args.Title)
		go kb.IndexNote(noteID, app)

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
		results, err := kb.Query(args.Query)
		if err != nil {
			return nil, nil, fmt.Errorf("kb_query: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: QueryResultsJSON(results)},
			},
		}, nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "kb_fetch",
		Description: "Fetch the full content of a note by its path. Use paths returned by kb_query. Multiple notes can be fetched in parallel.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args kbFetchArgs) (*mcp.CallToolResult, any, error) {
		slog.Debug("kb_fetch called", "path", args.Path)
		content, err := kb.ReadNoteContent(args.Path)
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
		note, err := kb.getNoteByPath(args.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("kb_touch: note not found: %w", err)
		}

		app.Indexing.add(note.id, note.title)
		go kb.TouchNote(note.id, app)

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: fmt.Sprintf("Touched: %s (last_verified updated to today)", note.title)},
			},
		}, nil, nil
	})

	return server
}

// setupHTTPMux creates an http.ServeMux with all routes registered.
func setupHTTPMux(app *App, kb *KnowledgeBase) *http.ServeMux {
	sseHandler := mcp.NewSSEHandler(func(r *http.Request) *mcp.Server {
		return newMCPServer(app, kb)
	}, nil)

	mux := http.NewServeMux()
	mux.Handle("/sse", sseHandler)
	mux.HandleFunc("/api/state", handleState(app))
	mux.HandleFunc("/api/sessions", handleSessions(app))
	mux.HandleFunc("/api/config", handleConfig(app))
	mux.HandleFunc("/api/suspend", handleSuspend(app))
	mux.HandleFunc("/api/activate", handleActivate(app))
	mux.HandleFunc("/api/preview", handlePreview(app))
	mux.HandleFunc("/api/session-ended", handleSessionEnded(app))
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

		app.Sessions.create(req.ID, req.Mode, req.Indicator, req.Preview, req.WindowTarget)
		slog.Debug("session registered via API", "id", req.ID, "mode", req.Mode, "window", req.WindowTarget)

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

		app.Sessions.setStatus(sess.ID, "suspended")
		slog.Debug("session suspended via API", "id", sess.ID, "window", req.WindowTarget)

		if req.WindowTarget != "" {
			go tmuxGracefulClose(req.WindowTarget)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "suspended", "session_id": sess.ID})
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

// startHTTPServerInBackground starts the HTTP server on an OS-assigned port in a
// goroutine and returns the *http.Server and actual port for later use.
func startHTTPServerInBackground(app *App, kb *KnowledgeBase) (*http.Server, int, error) {
	mux := setupHTTPMux(app, kb)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
	kb, err := OpenKnowledgeBase()
	if err != nil {
		return fmt.Errorf("open knowledge base: %w", err)
	}
	defer kb.Close()

	go kb.BackfillSummaries()

	app := newApp()
	mux := setupHTTPMux(app, kb)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	slog.Info("daemon listening", "addr", ln.Addr().String())
	return http.Serve(ln, mux)
}
