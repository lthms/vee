package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DaemonCmd runs the Vee daemon (MCP server + HTTP dashboard).
type DaemonCmd struct {
	Zettelkasten bool `short:"z" help:"Enable the vee-zettelkasten tools." name:"zettelkasten"`
	Port         int  `short:"p" default:"2700" help:"Port for the HTTP server (MCP + dashboard)." name:"port"`
}

// modeTransition records a single mode change.
type modeTransition struct {
	Mode      string    `json:"mode"`
	Indicator string    `json:"indicator"`
	Timestamp time.Time `json:"timestamp"`
}

// modeTracker keeps an in-memory log of mode transitions.
type modeTracker struct {
	mu               sync.RWMutex
	currentMode      string
	currentIndicator string
	transitions      []modeTransition
}

// toolTrace records a single tool call received from a hook.
type toolTrace struct {
	Tool      string    `json:"tool"`
	Input     any       `json:"input,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// toolTracer keeps an in-memory log of tool calls.
type toolTracer struct {
	mu     sync.RWMutex
	traces []toolTrace
}

func newToolTracer() *toolTracer {
	return &toolTracer{}
}

func (t *toolTracer) record(tool string, input any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.traces = append(t.traces, toolTrace{
		Tool:      tool,
		Input:     input,
		Timestamp: time.Now(),
	})
}

func (t *toolTracer) snapshot() []toolTrace {
	t.mu.RLock()
	defer t.mu.RUnlock()
	cp := make([]toolTrace, len(t.traces))
	copy(cp, t.traces)
	return cp
}

func newModeTracker() *modeTracker {
	initial := modeTransition{Mode: "idle", Indicator: "💤", Timestamp: time.Now()}
	return &modeTracker{
		currentMode:      "idle",
		currentIndicator: "💤",
		transitions:      []modeTransition{initial},
	}
}

func (t *modeTracker) record(mode, indicator string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.currentMode = mode
	t.currentIndicator = indicator
	t.transitions = append(t.transitions, modeTransition{
		Mode:      mode,
		Indicator: indicator,
		Timestamp: time.Now(),
	})
}

func (t *modeTracker) snapshot() (string, string, []modeTransition) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	cp := make([]modeTransition, len(t.transitions))
	copy(cp, t.transitions)
	return t.currentMode, t.currentIndicator, cp
}

// MCP tool args

type traverseArgs struct {
	KBRoot string `json:"kb_root" jsonschema:"Absolute path to the knowledge base root"`
	Topic  string `json:"topic" jsonschema:"The subject to search for"`
}

type reportModeChangeArgs struct {
	Mode      string `json:"mode" jsonschema:"The name of the mode being switched to (e.g. normal, vibe, contradictor)"`
	Indicator string `json:"indicator" jsonschema:"The emoji indicator for the mode (e.g. 🦊, ⚡, 😈)"`
}

// newServer creates a fresh MCP server with all tools registered.
// Called once per SSE connection so each session gets its own initialization lifecycle.
func (cmd *DaemonCmd) newServer(tracker *modeTracker) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "vee",
		Version: "1.0.0",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "report_mode_change",
		Description: "Report a mode transition. Call this every time you switch modes.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args reportModeChangeArgs) (*mcp.CallToolResult, any, error) {
		slog.Debug("report_mode_change called", "mode", args.Mode, "indicator", args.Indicator)
		tracker.record(args.Mode, args.Indicator)
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: fmt.Sprintf("Mode changed to %s.", args.Mode)},
			},
		}, nil, nil
	})

	if cmd.Zettelkasten {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "kb_traverse",
			Description: "Traverse a knowledge base index tree to find notes relevant to a topic. Returns a JSON array of {path, summary} pairs.",
		}, handleTraverse)
	}

	return server
}

// Run starts the daemon: MCP server (SSE) + dashboard on the same HTTP port.
func (cmd *DaemonCmd) Run() error {
	tracker := newModeTracker()
	tracer := newToolTracer()

	sseHandler := mcp.NewSSEHandler(func(r *http.Request) *mcp.Server {
		return cmd.newServer(tracker)
	}, nil)

	mux := http.NewServeMux()
	mux.Handle("/sse", sseHandler)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(dashboardHTML))
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		currentMode, currentIndicator, transitions := tracker.snapshot()
		traces := tracer.snapshot()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"current_mode":      currentMode,
			"current_indicator": currentIndicator,
			"transitions":       transitions,
			"tool_traces":       traces,
		})
	})
	mux.HandleFunc("/api/mode", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Mode      string `json:"mode"`
			Indicator string `json:"indicator"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		tracker.record(body.Mode, body.Indicator)
		slog.Debug("mode change via HTTP", "mode", body.Mode, "indicator", body.Indicator)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/tool-trace", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Tool  string `json:"tool_name"`
			Input any    `json:"tool_input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		tracer.record(body.Tool, body.Input)
		slog.Debug("tool-trace recorded", "tool", body.Tool)
		w.WriteHeader(http.StatusNoContent)
	})

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
  .timeline { list-style: none; }
  .timeline li {
    display: flex; align-items: center; gap: 1rem;
    padding: 0.5rem 0; border-bottom: 1px solid var(--border);
  }
  .timeline li:last-child { border-bottom: none; }
  .timeline .time { color: #565f89; font-size: 0.8rem; min-width: 8rem; }
  .timeline .mode-name { color: var(--fg); }
  .timeline .indicator { font-size: 1.2rem; }
  h2 { color: #565f89; font-size: 0.85rem; text-transform: uppercase; letter-spacing: 0.1em; margin-bottom: 0.75rem; }
  .tool-traces { list-style: none; }
  .tool-traces li {
    display: flex; align-items: center; gap: 1rem;
    padding: 0.5rem 0; border-bottom: 1px solid var(--border);
  }
  .tool-traces li:last-child { border-bottom: none; }
  .tool-traces .time { color: #565f89; font-size: 0.8rem; min-width: 8rem; }
  .tool-traces .tool-name { color: var(--accent); font-weight: bold; }
</style>
</head>
<body>
  <h1>Vee Dashboard</h1>
  <div class="current-mode">
    <div class="label">Current Mode</div>
    <div class="mode" id="current"></div>
  </div>
  <h2>Tool Traces</h2>
  <ul class="tool-traces" id="traces"></ul>
  <h2 style="margin-top: 2rem;">Mode History</h2>
  <ul class="timeline" id="timeline"></ul>
  <script>
    function render(data) {
      const ind = data.current_indicator || "❓";
      document.getElementById("current").textContent = ind + " " + data.current_mode;

      const tul = document.getElementById("traces");
      tul.innerHTML = "";
      const traces = [...(data.tool_traces || [])].reverse();
      for (const t of traces) {
        const li = document.createElement("li");
        const ti = new Date(t.timestamp).toLocaleTimeString();
        li.innerHTML = '<span class="time">' + ti + '</span><span class="tool-name">' + t.tool + '</span>';
        tul.appendChild(li);
      }

      const ul = document.getElementById("timeline");
      ul.innerHTML = "";
      const items = [...data.transitions].reverse();
      for (const t of items) {
        const li = document.createElement("li");
        const ti = new Date(t.timestamp).toLocaleTimeString();
        const mInd = t.indicator || "❓";
        li.innerHTML = '<span class="time">' + ti + '</span><span class="indicator">' + mInd + '</span><span class="mode-name">' + t.mode + '</span>';
        ul.appendChild(li);
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

