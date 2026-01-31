package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type traverseArgs struct {
	KBRoot string `json:"kb_root" jsonschema:"Absolute path to the knowledge base root"`
	Topic  string `json:"topic" jsonschema:"The subject to search for"`
}

func runMCPServer(zettelkasten bool) error {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "vee",
		Version: "1.0.0",
	}, nil)

	if zettelkasten {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "kb_traverse",
			Description: "Traverse a knowledge base index tree to find notes relevant to a topic. Returns a JSON array of {path, summary} pairs.",
		}, handleTraverse)
	}

	slog.Debug("starting MCP server", "zettelkasten", zettelkasten)
	return server.Run(context.Background(), &mcp.StdioTransport{})
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

func ensureMCPServer(zettelkasten bool) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	// Build the expected command line for the MCP server
	mcpArgs := []string{"mcp"}
	if zettelkasten {
		mcpArgs = append(mcpArgs, "-z")
	}
	expectedCmd := self

	// Check if already configured with the correct binary path
	cmd := exec.Command("claude", "mcp", "get", "vee")
	output, err := cmd.Output()
	if err == nil {
		// Parse the Command: line from the output
		for _, line := range strings.Split(string(output), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Command:") {
				registered := strings.TrimSpace(strings.TrimPrefix(line, "Command:"))
				if registered == expectedCmd {
					slog.Debug("MCP server already configured", "command", expectedCmd)
					return nil
				}
				slog.Debug("MCP server command mismatch, re-registering", "registered", registered, "expected", expectedCmd)
				// Remove stale config before re-adding
				rmCmd := exec.Command("claude", "mcp", "remove", "vee", "-s", "local")
				_ = rmCmd.Run()
				break
			}
		}
	}

	fmt.Printf("Configuring Vee MCP server...\n")
	addArgs := append([]string{"mcp", "add", "vee", self}, mcpArgs...)
	addCmd := exec.Command("claude", addArgs...)
	if err := addCmd.Run(); err != nil {
		return fmt.Errorf("failed to configure MCP server: %w", err)
	}

	return nil
}
