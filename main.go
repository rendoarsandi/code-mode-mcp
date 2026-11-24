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

Tools:
  code_read  -> STRICTLY READ-ONLY (fs.glob/stat/read/readLines/readFn/grep, git.status/diff).
                Safe to auto-approve; zero write or shell blast radius.
  code_exec  -> Full read/write/shell with cross-file atomic staging (commits to disk only if
                the script completes without error, or before sh.run / explicit fs.commit()).

Globals:
  print(...args) / console.log(...args)  -> append to capped stdout

fs (workspace-jailed to cwd):
  fs.glob(pattern?, maxFiles?)           -> string[] (supports ** and {a,b}; skips .git, node_modules, etc.)
  fs.stat(path)                          -> { exists, path, isDir, size, lines? }
  fs.read(path, startLine?, endLine?)    -> string (1-indexed inclusive line slice)
  fs.readLines(path, startLine?, end?)   -> string (1-indexed numbered lines "  12: ...")
  fs.readFn(path, symbol, maxLines?)     -> { found, file, symbol, startLine, endLine, text, suggestions? }
  fs.grep(regex, globPattern?, maxHits?) -> [{ file, line, text }] (accepts string or /RegExp/i; validates arg order)
  fs.edit(path, target, replacement, opts?)
                                         -> { changed, occurrences, file, dryRun, diff? }
                                            Throws if target matches 0 times (unless opts.optional)
                                            or >1 times (unless opts.replaceAll).
  fs.edit(path, [{ target, replacement, optional?, replaceAll? }], opts?)
                                         -> atomic multi-edit
  fs.editLines(path, startLine, endLine, replacement, opts?)
                                         -> replace 1-indexed inclusive line range (avoids escaping giant targets;
                                            optional opts.expectStart substring check on startLine)
  fs.write(path, content)                -> { written, dryRun, file, bytes }
  fs.commit()                            -> flush staged atomic edits to disk immediately (auto-called on clean exit & before sh.run)

git:
  git.status()                           -> string (git status --short)
  git.diff(statOnly?)                    -> string (git diff [--stat])

sh (code_exec only, when allowShell !== false):
  sh.run(command)                        -> { code, stdout, stderr }

Notes:
  - Cross-file edits are ATOMIC by default (atomic: true): if file #2 throws, file #1 is rolled back and disk stays untouched.
  - Set dryRun: true to preview fs.edit / fs.editLines / fs.write diffs without modifying disk.
  - diffMode: "auto" (default: compact ±3-line unified diff for small edits within inline budget, auto-falling back to stat), "stat", "compact", "full", or "none".
  - Set allowShell: false for pure read/edit sandboxing with zero shell execution.
  - Timeout defaults to 15000ms (max 120000ms); stdout defaults to 7500 bytes (max 65536) to stay inline without spillover.`

func toolsList() []map[string]any {
	return []map[string]any{
		{
			"name":        "code_read",
			"description": "Execute a STRICTLY READ-ONLY sandboxed JavaScript script (Goja VM) for fast multi-file glob, grep, stat, read, readLines, readFn, and git.status/diff. No file writes and no shell execution — safe to auto-approve.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"code": map[string]any{
						"type":        "string",
						"description": "Read-only JavaScript source. Globals: print, console.log, fs.{glob,stat,read,readLines,readFn,grep}, git.{status,diff}.",
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Workspace root directory (paths are jailed inside this directory). Defaults to server working directory.",
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
			"name":        "code_exec",
			"description": "Execute a sandboxed JavaScript script in a Go-hosted JS VM (Goja) to batch multi-file reads, atomic cross-file edits (fs.edit, fs.editLines, fs.write with automatic rollback on error, diffs & dryRun), git checks, and shell commands in one tool call.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"code": map[string]any{
						"type":        "string",
						"description": "JavaScript source to execute. Globals: print, console.log, fs.{glob,stat,read,readLines,readFn,grep,edit,editLines,write,commit}, git.{status,diff}, sh.run. Cross-file edits are staged atomically and rolled back on any uncaught error.",
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Workspace root directory (paths are jailed inside this directory). Defaults to server working directory.",
					},
					"dryRun": map[string]any{
						"type":        "boolean",
						"description": "If true, fs.edit / fs.editLines / fs.write compute and return unified diffs without modifying files on disk.",
					},
					"atomic": map[string]any{
						"type":        "boolean",
						"description": "Stage all file edits in memory and commit to disk only if the script succeeds without error (default true). Also auto-commits before sh.run().",
					},
					"diffMode": map[string]any{
						"type":        "string",
						"enum":        []string{"auto", "stat", "compact", "full", "none"},
						"description": "Controls diff verbosity: 'auto' (default: compact ±3-line unified diff for small edits within inline budget, falling back to stat), 'stat' (one-line +/- summary), 'compact' (up to 20 changed lines ±3 ctx), 'full' (up to 60 changed lines), or 'none'.",
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
			"description": "Return the concise API reference for the sandboxed JavaScript helpers available in code_read and code_exec.",
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
		case "code_read":
			var execReq ExecRequest
			if len(params.Arguments) > 0 {
				if err := json.Unmarshal(params.Arguments, &execReq); err != nil {
					return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: "invalid arguments: " + err.Error()}}
				}
			}
			allowFalse := false
			execReq.ReadOnly = true
			execReq.DryRun = false
			execReq.AllowShell = &allowFalse
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
