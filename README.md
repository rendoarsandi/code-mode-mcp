# code-mode-mcp

A lightweight, pure-Go **Code Mode MCP Server** (stdio JSON-RPC 2.0) powered by an embedded JavaScript VM ([Goja](https://github.com/dop251/goja)).

Instead of making dozens of sequential tool calls (`read_file`, `grep`, `edit_file`), an AI agent sends a single JS script to `code_exec`, processes files and filters noise locally, and receives only the distilled output + unified diffs.

## Tools Exposed
- `code_exec` — Execute JS inside a workspace-jailed Goja VM with:
  - `print(...)` / `console.log(...)` (hard byte-capped stdout)
  - `fs.glob(pattern?, maxFiles?)` (supports `**` and `{a,b}` brace alternation)
  - `fs.stat(path)`
  - `fs.read(path, startLine?, endLine?)`
  - `fs.grep(regex, globPattern?, maxHits?)`
  - `fs.edit(path, target, replacement)` (exact-match + `diffMode: "stat" | "compact" | "full" | "none"` + `dryRun`)
  - `fs.write(path, content)` (`dryRun` + `diffMode` support)
  - `git.status()` / `git.diff(statOnly?)`
  - `sh.run(command)` (optional, can be disabled via `allowShell: false`)
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
