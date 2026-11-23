package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	serverName    = "code-mode-mcp"
	serverVersion = "0.1.0"
	protocolVer   = "2024-11-05"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const apiReference = `Code Mode MCP — JavaScript Host API (runs in sandboxed Goja JS VM):

Globals:
  print(...args) / console.log(...args)  -> append to capped stdout

fs (workspace-jailed to cwd):
  fs.glob(pattern?, maxFiles?)           -> string[] (skips .git, node_modules, dist, build, etc.)
  fs.stat(path)                          -> { exists, path, isDir, size, lines? }
  fs.read(path, startLine?, endLine?)    -> string (1-indexed inclusive line slice)
  fs.grep(regex, globPattern?, maxHits?) -> [{ file, line, text }]
  fs.edit(path, target, replacement)     -> { changed, occurrences, file, dryRun, diff }
  fs.write(path, content)                -> { written, dryRun, file, bytes }

git:
  git.status()                           -> string (git status --short)
  git.diff(statOnly?)                    -> string (git diff [--stat])

sh (when allowShell !== false):
  sh.run(command)                        -> { code, stdout, stderr }

Notes:
  - Set dryRun: true to preview fs.edit / fs.write diffs without modifying disk.
  - diffMode: "stat" (default for writes), "compact" (default for dryRun), "full", or "none" to keep multi-edit responses inline.
  - Set allowShell: false for pure read/edit sandboxing with zero shell execution.
  - Timeout defaults to 15000ms (max 120000ms); stdout defaults to 7500 bytes (max 65536) to stay inline without spillover.`

func toolsList() []map[string]any {
	return []map[string]any{
		{
			"name":        "code_exec",
			"description": "Execute a sandboxed JavaScript script in a Go-hosted JS VM (Goja) to batch multi-file searches, reads, filtering, edits (with automatic diffs & dryRun), git checks, and shell commands in one tool call. Returns only distilled stdout/result + diffs.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"code": map[string]any{
						"type":        "string",
						"description": "JavaScript source to execute. Available globals: print, console.log, fs.{glob,stat,read,grep,edit,write}, git.{status,diff}, sh.run.",
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Workspace root directory (paths are jailed inside this directory). Defaults to server working directory.",
					},
					"dryRun": map[string]any{
						"type":        "boolean",
						"description": "If true, fs.edit and fs.write compute and return unified diffs without modifying files on disk.",
					},
					"diffMode": map[string]any{
						"type":        "string",
						"enum":        []string{"stat", "compact", "full", "none"},
						"description": "Controls diff verbosity in the response: 'stat' (default for writes, one-line +/- summary), 'compact' (default for dryRun, up to 12 changed lines), 'full' (up to 60 changed lines), or 'none' (omit diffs).",
					},
					"allowShell": map[string]any{
						"type":        "boolean",
						"description": "Allow sh.run(cmd) inside the script (default true). Set false for pure sandboxed fs/git execution.",
					},
					"timeoutMs": map[string]any{
						"type":        "integer",
						"description": "Execution timeout in milliseconds (default 15000, max 120000).",
					},
					"maxOutputBytes": map[string]any{
						"type":        "integer",
						"description": "Maximum stdout bytes returned (default 7500 to stay inline, max 65536).",
					},
				},
				"required": []string{"code"},
			},
		},
		{
			"name":        "code_api",
			"description": "Return the concise API reference for the sandboxed JavaScript helpers available in code_exec.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

func handleRPC(req rpcRequest) *rpcResponse {
	switch req.Method {
	case "initialize":
		return &rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": protocolVer,
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    serverName,
					"version": serverVersion,
				},
			},
		}
	case "notifications/initialized":
		return nil
	case "ping":
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}
	case "tools/list":
		return &rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{"tools": toolsList()},
		}
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}}
		}
		switch params.Name {
		case "code_api":
			return &rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]any{
					"content": []map[string]any{{"type": "text", "text": apiReference}},
				},
			}
		case "code_exec":
			var execReq ExecRequest
			if len(params.Arguments) > 0 {
				if err := json.Unmarshal(params.Arguments, &execReq); err != nil {
					return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: "invalid arguments: " + err.Error()}}
				}
			}
			execRes := runCodeExec(execReq)
			outJSON, _ := json.MarshalIndent(execRes, "", "  ")
			isErr := execRes.Error != ""
			return &rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]any{
					"content": []map[string]any{{"type": "text", "text": string(outJSON)}},
					"isError": isErr,
				},
			}
		default:
			return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: "unknown tool: " + params.Name}}
		}
	default:
		if len(req.ID) == 0 || string(req.ID) == "null" {
			return nil // ignore unknown notifications
		}
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + req.Method}}
	}
}

func serveStdio(r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	enc := json.NewEncoder(w)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			_ = enc.Encode(rpcResponse{
				JSONRPC: "2.0",
				Error:   &rpcError{Code: -32700, Message: "parse error: " + err.Error()},
			})
			continue
		}
		if resp := handleRPC(req); resp != nil {
			if err := enc.Encode(resp); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func main() {
	if err := serveStdio(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "code-mode-mcp fatal: %v\n", err)
		os.Exit(1)
	}
}
