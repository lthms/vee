package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/lthms/vee/internal/feedback"
	"github.com/lthms/vee/internal/kb"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP tool args

type kbRememberArgs struct {
	Content    string `json:"content" jsonschema:"The statement to save. Must be a single atomic fact (max 2000 chars)."`
	Source     string `json:"source" jsonschema:"Origin of the information (file path, URL, issue reference, etc.)"`
	SourceType string `json:"source_type,omitempty" jsonschema:"Type of source (default: manual)"`
	Scope      string `json:"scope,omitempty" jsonschema:"Scope: user (all projects) or project (this project only). Default: user"`
}

type kbQueryArgs struct {
	Query string `json:"query" jsonschema:"Search query. Use specific, meaningful search terms (e.g. 'tmux keybindings'). Do NOT use wildcards or glob patterns."`
}

type kbTouchArgs struct {
	ID string `json:"id" jsonschema:"Statement ID (as returned by kb_query)"`
}

type kbForgetArgs struct {
	ID string `json:"id" jsonschema:"Statement ID to flag for deletion (as returned by kb_query)"`
}

type feedbackRecordArgs struct {
	Kind      string `json:"kind" jsonschema:"Whether this is a good or bad example (good or bad)"`
	Statement string `json:"statement" jsonschema:"The example or counter-example statement"`
	Scope     string `json:"scope" jsonschema:"Scope: user (all projects) or project (this project only)"`
}

// newMCPServer creates a fresh MCP server with only the enabled tools.
// Called once per SSE connection so each session gets its own initialization lifecycle.
func newMCPServer(kbase *kb.KnowledgeBase, fstore *feedback.Store, profile string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "vee",
		Version: "1.0.0",
	}, nil)

	if kbase != nil {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "kb_remember",
			Description: "Save a fact to your long-term memory. Use this whenever you learn something useful: build commands, project conventions, user preferences, architectural decisions. One atomic fact per call. Will be deduplicated automatically.",
		}, func(ctx context.Context, req *mcp.CallToolRequest, args kbRememberArgs) (*mcp.CallToolResult, any, error) {
			slog.Debug("kb_remember called")

			scope := args.Scope
			if scope == "" {
				scope = "user"
			}
			if scope != "user" && scope != "project" {
				return &mcp.CallToolResult{
					Content: []mcp.Content{
						&mcp.TextContent{Text: "scope must be 'user' or 'project'"},
					},
					IsError: true,
				}, nil, nil
			}

			project := ""
			if scope == "project" {
				project, _ = os.Getwd()
			}

			result, err := kbase.AddStatement(args.Content, args.Source, args.SourceType, scope, project)
			if err != nil {
				return nil, nil, fmt.Errorf("kb_remember: %w", err)
			}

			msg := fmt.Sprintf("Statement saved (id: %s, scope: %s, status: pending — will be promoted after duplicate check)", result.ID, scope)

			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: msg},
				},
			}, nil, nil
		})

		mcp.AddTool(server, &mcp.Tool{
			Name:        "kb_query",
			Description: "Search your long-term memory. Query BEFORE starting unfamiliar tasks — past sessions may have already solved this. Use specific terms like 'vee build commands' not vague phrases. Empty results = opportunity to learn and remember.",
		}, func(ctx context.Context, req *mcp.CallToolRequest, args kbQueryArgs) (*mcp.CallToolResult, any, error) {
			slog.Debug("kb_query called", "query", args.Query)
			project, _ := os.Getwd()
			results, err := kbase.Query(args.Query, project)
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
			Name:        "kb_touch",
			Description: "Confirm a statement is still accurate. Call this when you use information from kb_query and verify it's correct. Keeps the knowledge base healthy by tracking freshness.",
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

		mcp.AddTool(server, &mcp.Tool{
			Name:        "kb_forget",
			Description: "Flag outdated or incorrect information for removal. Use when you discover a kb_query result is wrong or obsolete. Hidden immediately, queued for user to confirm deletion.",
		}, func(ctx context.Context, req *mcp.CallToolRequest, args kbForgetArgs) (*mcp.CallToolResult, any, error) {
			slog.Debug("kb_forget called", "id", args.ID)
			if err := kbase.FlagStatement(args.ID); err != nil {
				return nil, nil, fmt.Errorf("kb_forget: %w", err)
			}

			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: fmt.Sprintf("Flagged for deletion: %s (pending user review)", args.ID)},
				},
			}, nil, nil
		})
	}

	if fstore != nil {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "feedback_record",
			Description: "Record a good or bad example of profile behavior. The profile is inferred automatically from the current session.",
		}, func(ctx context.Context, req *mcp.CallToolRequest, args feedbackRecordArgs) (*mcp.CallToolResult, any, error) {
			slog.Debug("feedback_record called", "profile", profile)

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

			project, _ := os.Getwd()

			id, err := fstore.Record(profile, args.Kind, args.Statement, args.Scope, project)
			if err != nil {
				return nil, nil, fmt.Errorf("feedback_record: %w", err)
			}

			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: fmt.Sprintf("Feedback recorded (id: %s, profile: %s, kind: %s, scope: %s)", id, profile, args.Kind, args.Scope)},
				},
			}, nil, nil
		})
	}

	return server
}

// startMCPSidecar starts a lightweight HTTP server hosting only the MCP SSE endpoint
// and (when ephemeral) a GPG signing endpoint. It returns the port and blocks until
// ctx is cancelled.
func startMCPSidecar(ctx context.Context, kbase *kb.KnowledgeBase, fstore *feedback.Store, profile string, ephemeral bool) (int, error) {
	sseHandler := mcp.NewSSEHandler(func(r *http.Request) *mcp.Server {
		return newMCPServer(kbase, fstore, profile)
	}, nil)

	mux := http.NewServeMux()
	mux.Handle("/sse", sseWithKeepalive(sseHandler, defaultSSEKeepaliveInterval))

	if ephemeral {
		mux.HandleFunc("/api/gpg/sign", handleGPGSign())
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("listen: %w", err)
	}

	port := ln.Addr().(*net.TCPAddr).Port

	srv := &http.Server{Handler: mux}
	go func() {
		slog.Info("mcp sidecar listening", "addr", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("mcp sidecar error", "error", err)
		}
	}()
	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	return port, nil
}

// handleGPGSign handles POST /api/gpg/sign — signs data using the host's GPG.
// Request body contains the data to sign. Query params: key (signing key ID).
// Returns the detached armored signature.
func handleGPGSign() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		key := r.URL.Query().Get("key")
		if key == "" {
			http.Error(w, "missing key query parameter", http.StatusBadRequest)
			return
		}

		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read request body: "+err.Error(), http.StatusBadRequest)
			return
		}

		cmd := exec.Command("gpg", "--batch", "--yes", "-bsau", key)
		cmd.Stdin = strings.NewReader(string(data))

		output, err := cmd.Output()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				slog.Error("gpg signing failed", "error", err, "stderr", string(exitErr.Stderr))
				http.Error(w, "gpg signing failed: "+string(exitErr.Stderr), http.StatusInternalServerError)
			} else {
				http.Error(w, "gpg signing failed: "+err.Error(), http.StatusInternalServerError)
			}
			return
		}

		w.Header().Set("Content-Type", "application/pgp-signature")
		w.Write(output)
	}
}
