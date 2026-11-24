package main

import (
	"bufio"
	"bytes"
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
  code_read  -> STRICTLY READ-ONLY (fs.glob/stat/read/readLines/symbols/readFn/readFnAt/grep, git.status/diff).
                Safe to auto-approve; zero write or shell blast radius.
  code_exec  -> Full read/write/shell with cross-file atomic staging (commits to disk only if
                the script completes without error, or before sh.run / explicit fs.commit()).

Globals:
  print(...args) / console.log(...args)  -> append to capped plain-text stdout (head+tail preserved on truncation)
  out.{used,limit,remaining,truncated}() -> inspect remaining stdout byte budget in JS

fs (workspace-jailed to cwd):
  fs.glob(pattern?, optsOrMaxFiles?)     -> string[] (supports ** and {a,b}; opts: { maxFiles, ignore }; .truncated flag)
  fs.stat(path)                          -> { exists, path, isDir, size, lines? }
  fs.read(path, startLine?, endLine?)    -> string (1-indexed inclusive line slice)
  fs.readLines(path, startLine?, end?)   -> string (1-indexed numbered lines "  12: ...")
  fs.symbols(path)                       -> [{ name, kind, line, endLine }] (file outline without reading body)
  fs.readFn(path, symbol, maxLines?)     -> { found, file, symbol, startLine, endLine, text, matches?, suggestions? }
  fs.readFnAt(path, line, maxLines?)     -> { found, file, symbol, startLine, endLine, text }
                                            Returns innermost enclosing function/class around 1-based line
                                            (or ±15 lines fallback context in text with found:false if top-level).
  fs.grep(regex, glob?, optsOrMaxHits?)  -> [{ file, line, text }] (string or /RegExp/i; opts: { maxHits, ignore }; .truncated)
  fs.grepFn(regex, glob?, optsOrMaxFns?) -> [{ found, file, symbol, startLine, endLine, hitLines, text }]
                                            One-liner grep + deduplicated enclosing functions (default maxFns=8)
  fs.edit(path, target, replacement, opts?)
                                         -> { changed, occurrences, file, dryRun, diff? }
                                            opts: { optional?, replaceAll?, wholeWord?, expectCount?, maxReplacements? }
  fs.edit(path, [{ target, replacement, ... }], opts?)
                                         -> atomic multi-edit
  fs.editLines(path, startLine, endLine, replacement, opts?)
                                         -> replace 1-indexed inclusive line range (opts: { expectStart?, expectEnd? })
  fs.write(path, content)                -> { written, dryRun, file, bytes }
  fs.commit()                            -> flush staged atomic edits to disk immediately (auto-called on clean exit & before sh.run)
  fs.revert()                            -> restore all files touched in this script back to their pre-script disk state

git:
  git.status()                           -> string (git status --short)
  git.diff(statOnly?)                    -> string (git diff [--stat])

sh (code_exec only, when allowShell !== false):
  sh.run(command)                        -> { code, stdout, stderr, timedOut }
  sh.check(command, optsOrFilterRegex?)  -> { ok, code, timedOut, totalLines, matchCount, matches, tail }

Notes:
  - Cross-file edits are ATOMIC by default (atomic: true): if file #2 throws, file #1 is rolled back and disk stays untouched.
  - Truncated outputs preserve head+tail and return an outputId; pass { outputId, outputOffset } to code_read to page through the dropped bytes without re-running the script.
  - Set dryRun: true to preview fs.edit / fs.editLines / fs.write diffs without modifying disk.
  - diffMode: "auto" (default: compact ±3-line unified diff for small edits within inline budget, auto-falling back to stat), "stat", "compact", "full", or "none".
  - Set allowShell: false for pure read/edit sandboxing with zero shell execution.
  - Timeout defaults to 15000ms for code_read / 45000ms for code_exec (max 300000ms); stdout defaults to 9200 bytes (max 65536) in plain text to stay inline without spillover.`

const serverInstructions = "Use code_read for strictly read-only multi-file discovery, audits, symbols/readFn/readFnAt/grepFn, and git checks. Use code_exec for builds/tests via sh.run/sh.check or atomic bulk edits. Keep stdout concise (default 9200-byte plain-text cap with head+tail preservation and outputId pagination); use native read_file for large >150-line verbatim reads."

func toolsList() []map[string]any {
	return []map[string]any{
		{
			"name":        "code_read",
			"description": "Execute a STRICTLY READ-ONLY sandboxed JavaScript script (Goja VM) for fast multi-file glob, grep, grepFn, stat, read, readLines, symbols, readFn, readFnAt, and git.status/diff. No file writes and no shell execution — safe to auto-approve.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"code": map[string]any{
						"type":        "string",
						"description": "Read-only JavaScript source (omit when reading a cached chunk via outputId). Globals: print, console.log, out.{used,limit,remaining,truncated}, fs.{glob,stat,read,readLines,symbols,readFn,readFnAt,grep,grepFn}, git.{status,diff}.",
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Workspace root directory (paths are jailed inside this directory). Defaults to server working directory.",
					},
					"timeoutMs": map[string]any{
						"type":        "integer",
						"description": "Execution timeout in milliseconds (default 15000, max 300000).",
					},
					"maxOutputBytes": map[string]any{
						"type":        "integer",
						"description": "Maximum stdout bytes returned (default 9200 to stay inline, max 65536).",
					},
					"outputId": map[string]any{
						"type":        "string",
						"description": "Cached outputId from a previously truncated call to read the next byte chunk without re-running the script.",
					},
					"outputOffset": map[string]any{
						"type":        "integer",
						"description": "Byte offset into cached outputId (default 0).",
					},
				},
			},
		},
		{
			"name":        "code_exec",
			"description": "Execute a sandboxed JavaScript script in a Go-hosted JS VM (Goja) to batch multi-file reads, atomic cross-file edits (fs.edit with wholeWord/expectCount/maxReplacements, fs.editLines with expectStart/expectEnd, fs.write, fs.revert, net-per-file diffs & dryRun), git checks, and shell commands (sh.run, sh.check) in one tool call.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"code": map[string]any{
						"type":        "string",
						"description": "JavaScript source to execute. Globals: print, console.log, out.{used,limit,remaining,truncated}, fs.{glob,stat,read,readLines,symbols,readFn,readFnAt,grep,grepFn,edit,editLines,write,commit,revert}, git.{status,diff}, sh.{run,check}. Cross-file edits are staged atomically and rolled back on any uncaught error.",
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
						"description": "Allow sh.run/sh.check inside the script (default true). Set false for pure sandboxed fs/git execution.",
					},
					"timeoutMs": map[string]any{
						"type":        "integer",
						"description": "Execution timeout in milliseconds (default 45000 for builds/tests, max 300000).",
					},
					"maxOutputBytes": map[string]any{
						"type":        "integer",
						"description": "Maximum stdout bytes returned (default 9200 to stay inline, max 65536).",
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

func marshalCompactNoEscape(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprintf("%v", v)
	}
	return strings.TrimRight(buf.String(), "\n")
}

// formatExecResult renders ExecResult as compact plain text without JSON-escaping stdout/diffs.
func formatExecResult(res ExecResult) string {
	var b strings.Builder
	if res.Stdout != "" {
		b.WriteString(strings.TrimRight(res.Stdout, "\n"))
		b.WriteByte('\n')
	}
	if res.Truncated {
		shown := res.ShownBytes
		if shown <= 0 {
			shown = len(res.Stdout)
		}
		dropped := res.TotalBytes - shown
		if dropped < 0 {
			dropped = 0
		}
		if res.OutputId != "" && res.NextOffset > 0 {
			fmt.Fprintf(&b, "[truncated: showed %d/%d bytes (%d dropped) | outputId=%s nextOffset=%d — call code_read({ outputId: %q, outputOffset: %d }) to continue]\n",
				shown, res.TotalBytes, dropped, res.OutputId, res.NextOffset, res.OutputId, res.NextOffset)
		} else if res.OutputId != "" {
			fmt.Fprintf(&b, "[truncated: showed %d/%d bytes (%d dropped) | outputId=%s — call code_read({ outputId: %q, outputOffset: %d }) to read more, or use fs.symbols/fs.readFn/fs.readFnAt / native read_file]\n",
				shown, res.TotalBytes, dropped, res.OutputId, res.OutputId, res.NextOffset)
		} else {
			fmt.Fprintf(&b, "[truncated: showed %d/%d bytes (%d dropped) — narrow output, use fs.symbols/fs.readFn/fs.readFnAt, or native read_file for large verbatim reads]\n",
				shown, res.TotalBytes, dropped)
		}
	}
	if res.Result != nil {
		var rendered string
		if s, ok := res.Result.(string); ok {
			rendered = s
		} else {
			rendered = marshalCompactNoEscape(res.Result)
		}
		// Bound res.Result against maxOut so returning a large expression cannot blow the inline budget.
		resBudget := res.maxOut
		if resBudget <= 0 {
			resBudget = defaultMaxOutByte
		}
		if remain := resBudget - len(res.Stdout); remain < resBudget {
			if remain < 1024 && resBudget >= 1024 {
				remain = 1024
			}
			if remain > 0 {
				resBudget = remain
			}
		}
		if len(rendered) > resBudget {
			origLen := len(rendered)
			rendered = trimUTF8Prefix(rendered, resBudget)
			b.WriteString("=> ")
			b.WriteString(rendered)
			b.WriteByte('\n')
			fmt.Fprintf(&b, "[result truncated: showed %d/%d bytes — print filtered fields or increase maxOutputBytes]\n", len(rendered), origLen)
		} else {
			b.WriteString("=> ")
			b.WriteString(rendered)
			b.WriteByte('\n')
		}
	}
	if len(res.Diffs) > 0 {
		b.WriteString("--- diffs ---\n")
		for _, d := range res.Diffs {
			b.WriteString(strings.TrimRight(d, "\n"))
			b.WriteByte('\n')
		}
	}
	if res.Error != "" {
		b.WriteString("--- error ---\n")
		b.WriteString(strings.TrimRight(res.Error, "\n"))
		b.WriteByte('\n')
	}
	meta := []string{fmt.Sprintf("%dms", res.DurationMs)}
	if res.DryRun {
		meta = append(meta, "dryRun=true")
	}
	if res.RolledBack {
		meta = append(meta, "rolledBack=true")
	}
	if len(res.EditedFiles) > 0 {
		meta = append(meta, "edited="+strings.Join(res.EditedFiles, ","))
	}
	b.WriteByte('[')
	b.WriteString(strings.Join(meta, " | "))
	b.WriteByte(']')
	return b.String()
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
				"instructions": serverInstructions,
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
			isErr := execRes.Error != ""
			return &rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]any{
					"content": []map[string]any{{"type": "text", "text": formatExecResult(execRes)}},
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
			isErr := execRes.Error != ""
			return &rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]any{
					"content": []map[string]any{{"type": "text", "text": formatExecResult(execRes)}},
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
