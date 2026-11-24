# code-mode-mcp

A lightweight, pure-Go **Code Mode MCP Server** (stdio JSON-RPC 2.0) powered by an embedded JavaScript VM ([Goja](https://github.com/dop251/goja)).

Instead of making dozens of sequential tool calls (`read_file`, `grep`, `edit_file`), an AI agent sends a single JS script to `code_exec`, processes files and filters noise locally, and receives only the distilled output + unified diffs.

## Tools Exposed
- `code_read` — **Strictly read-only** JS sandbox (`fs.glob`, `fs.stat`, `fs.read`, `fs.readLines`, `fs.readFn`, `fs.grep`, `git.status`, `git.diff`) with zero write or shell access—safe to auto-approve independently.
- `code_exec` — Execute JS inside a workspace-jailed Goja VM with **cross-file atomic staging** (rolls back all staged writes automatically if any edit throws), plus:
  - `print(...)` / `console.log(...)` (hard byte-capped stdout)
  - `fs.glob(pattern?, maxFiles?)` (supports `**` and `{a,b}` brace alternation)
  - `fs.stat(path)`
  - `fs.read(path, startLine?, endLine?)` / `fs.readLines(path, startLine?, endLine?)`
  - `fs.readFn(path, symbol, maxLines?)` (extracts full function/class body with 1-based line numbers, ±2 context lines, and closest-symbol `suggestions` on miss)
  - `fs.grep(regex, globPattern?, maxHits?)` (accepts string or JS `/RegExp/i`, validates swapped arguments)
  - `fs.edit(path, target, replacement, opts?)` / `fs.edit(path, [{ target, replacement }], opts?)` (strict unique match by default; `{ optional: true }`, `{ replaceAll: true }`, atomic multi-edit, per-hunk Myers diff with `diffMode: "auto" | "stat" | "compact" | "full" | "none"`, and `dryRun`)
  - `fs.editLines(path, startLine, endLine, replacement, opts?)` (1-indexed inclusive line-range replacement with optional `{ expectStart }` guard—avoids double-escaping template literals)
  - `fs.write(path, content)` / `fs.commit()`
  - `git.status()` / `git.diff(statOnly?)`
  - `sh.run(command)` (auto-commits staged edits first; optional, can be disabled via `allowShell: false`)
- `code_api` — Returns the concise JS helper reference.

## Build & Test
```bash
go test -v ./...
go build -o bin/code-mode-mcp .
```

## Antigravity MCP Config (`~/.gemini/config/mcp_config.json`)
```json
{
  "mcpServers": {
    "code-mode": {
      "command": "/data/data/com.termux/files/home/code-mode-mcp/bin/code-mode-mcp",
      "args": []
    }
  }
}
```
