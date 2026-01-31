package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DaemonCmd runs the Vee daemon (MCP server + HTTP dashboard).
type DaemonCmd struct {
	Zettelkasten bool `short:"z" help:"Enable the vee-zettelkasten tools." name:"zettelkasten"`
	Port         int  `short:"p" default:"2700" help:"Port for the HTTP server (MCP + dashboard)." name:"port"`
}

// MCP tool args

type traverseArgs struct {
	KBRoot string `json:"kb_root" jsonschema:"Absolute path to the knowledge base root"`
	Topic  string `json:"topic" jsonschema:"The subject to search for"`
}

type requestSuspendArgs struct{}
type selfDropArgs struct{}

// newMCPServer creates a fresh MCP server with all tools registered.
// Called once per SSE connection so each session gets its own initialization lifecycle.
func newMCPServer(app *App, zettelkasten bool) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "vee",
		Version: "1.0.0",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "request_suspend",
		Description: "Request that the current Vee session be suspended so it can be resumed later.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args requestSuspendArgs) (*mcp.CallToolResult, any, error) {
		slog.Debug("request_suspend called")
		if app.Control.requestSuspend() {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: "Suspend requested."},
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
		if app.Control.requestSelfDrop() {
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

	if zettelkasten {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "kb_traverse",
			Description: "Traverse a knowledge base index tree to find notes relevant to a topic. Returns a JSON array of {path, summary} pairs.",
		}, handleTraverse)
	}

	return server
}

// setupHTTPMux creates an http.ServeMux with all routes registered.
func setupHTTPMux(app *App, zettelkasten bool) *http.ServeMux {
	sseHandler := mcp.NewSSEHandler(func(r *http.Request) *mcp.Server {
		return newMCPServer(app, zettelkasten)
	}, nil)

	mux := http.NewServeMux()
	mux.Handle("/sse", sseHandler)
	mux.HandleFunc("/", handleDashboard)
	mux.HandleFunc("/api/state", handleState(app))
	return mux
}

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(dashboardHTML))
}

func handleState(app *App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		activeSession := app.Sessions.active()
		suspendedSessions := app.Sessions.suspended()
		completedSessions := app.Sessions.completed()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"active_session":     activeSession,
			"suspended_sessions": suspendedSessions,
			"completed_sessions": completedSessions,
		})
	}
}

// startHTTPServerInBackground starts the HTTP server on the given port in a
// goroutine and returns the *http.Server for later shutdown.
func startHTTPServerInBackground(app *App, port int, zettelkasten bool) (*http.Server, error) {
	mux := setupHTTPMux(app, zettelkasten)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}

	srv := &http.Server{Handler: mux}
	go func() {
		slog.Info("http server listening", "addr", addr)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("http server error", "error", err)
		}
	}()

	return srv, nil
}

// Run starts the daemon: MCP server (SSE) + dashboard on the same HTTP port.
func (cmd *DaemonCmd) Run() error {
	app := newApp()
	mux := setupHTTPMux(app, cmd.Zettelkasten)
	addr := fmt.Sprintf("127.0.0.1:%d", cmd.Port)
	slog.Info("daemon listening", "addr", addr)
	return http.ListenAndServe(addr, mux)
}

func handleTraverse(ctx context.Context, req *mcp.CallToolRequest, args traverseArgs) (*mcp.CallToolResult, any, error) {
	slog.Debug("kb_traverse called", "kb_root", args.KBRoot, "topic", args.Topic)

	result, err := traverseToJSON(ctx, args.KBRoot, args.Topic)
	if err != nil {
		return nil, nil, fmt.Errorf("traverse failed: %w", err)
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: result},
		},
	}, nil, nil
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Vee Dashboard</title>
<style>
  :root {
    --bg: #1a1b26; --fg: #a9b1d6; --accent: #7aa2f7;
    --card-bg: #24283b; --border: #414868;
    --green: #9ece6a; --yellow: #e0af68;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body {
    font-family: "Berkeley Mono", "JetBrains Mono", monospace;
    background: var(--bg); color: var(--fg);
    padding: 2rem; min-height: 100vh;
  }
  h1 { color: var(--accent); font-size: 1.4rem; margin-bottom: 1.5rem; }
  .current-mode {
    background: var(--card-bg); border: 1px solid var(--border);
    border-radius: 8px; padding: 1.5rem; margin-bottom: 2rem;
  }
  .current-mode .label { font-size: 0.85rem; color: #565f89; text-transform: uppercase; letter-spacing: 0.1em; }
  .current-mode .mode { font-size: 2rem; margin-top: 0.5rem; }
  .session-card {
    background: var(--card-bg); border: 1px solid var(--border);
    border-radius: 8px; padding: 1rem 1.5rem; margin-bottom: 1rem;
  }
  .session-card .session-header {
    display: flex; align-items: center; gap: 0.75rem; font-size: 1.1rem;
  }
  .session-card .session-meta {
    font-size: 0.8rem; color: #565f89; margin-top: 0.4rem;
  }
  .session-card .session-preview {
    font-size: 0.85rem; color: var(--fg); margin-top: 0.3rem;
    opacity: 0.8; font-style: italic;
  }
  .session-card.active { border-color: var(--green); }
  .session-card.suspended { border-color: var(--yellow); }
  .session-card.completed { border-color: #565f89; opacity: 0.7; }
  .empty-state { color: #565f89; font-size: 0.85rem; font-style: italic; margin-bottom: 1rem; }
  h2 { color: #565f89; font-size: 0.85rem; text-transform: uppercase; letter-spacing: 0.1em; margin-bottom: 0.75rem; }
</style>
</head>
<body>
  <h1>Vee Dashboard</h1>
  <div class="current-mode">
    <div class="label">Current Mode</div>
    <div class="mode" id="current"></div>
  </div>
  <h2>Active Session</h2>
  <div id="active-session"></div>
  <h2>Suspended Sessions</h2>
  <div id="suspended-sessions"></div>
  <h2>Completed Sessions</h2>
  <div id="completed-sessions"></div>
  <script>
    function age(ts) {
      const s = Math.floor((Date.now() - new Date(ts).getTime()) / 1000);
      if (s < 60) return s + "s";
      if (s < 3600) return Math.floor(s/60) + "m";
      return Math.floor(s/3600) + "h " + Math.floor((s%3600)/60) + "m";
    }
    function sessionCard(s, cls) {
      return '<div class="session-card ' + cls + '">' +
        '<div class="session-header"><span>' + s.indicator + '</span><span>' + s.mode + '</span></div>' +
        (s.preview ? '<div class="session-preview">' + s.preview + '</div>' : '') +
        '<div class="session-meta">' + age(s.started_at) + ' ago</div>' +
        '</div>';
    }
    function render(data) {
      const cur = document.getElementById("current");
      if (data.active_session) {
        cur.textContent = data.active_session.indicator + " " + data.active_session.mode;
      } else {
        cur.textContent = "💤 idle";
      }

      const aDiv = document.getElementById("active-session");
      if (data.active_session) {
        aDiv.innerHTML = sessionCard(data.active_session, "active");
      } else {
        aDiv.innerHTML = '<div class="empty-state">No active session</div>';
      }

      const sDiv = document.getElementById("suspended-sessions");
      const suspended = data.suspended_sessions || [];
      if (suspended.length === 0) {
        sDiv.innerHTML = '<div class="empty-state">No suspended sessions</div>';
      } else {
        sDiv.innerHTML = suspended.map(function(s) { return sessionCard(s, "suspended"); }).join("");
      }

      const cDiv = document.getElementById("completed-sessions");
      const completed = data.completed_sessions || [];
      if (completed.length === 0) {
        cDiv.innerHTML = '<div class="empty-state">No completed sessions</div>';
      } else {
        cDiv.innerHTML = completed.map(function(s) { return sessionCard(s, "completed"); }).join("");
      }

    }
    async function poll() {
      try {
        const r = await fetch("/api/state");
        render(await r.json());
      } catch(e) {}
    }
    poll();
    setInterval(poll, 2000);
  </script>
</body>
</html>`
