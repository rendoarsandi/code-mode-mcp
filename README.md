# code-mode-mcp

A lightweight, pure-Go **Code Mode MCP Server** (stdio JSON-RPC 2.0) powered by an embedded JavaScript VM ([Goja](https://github.com/dop251/goja)).

Instead of making dozens of sequential tool calls (`read_file`, `grep`, `edit_file`), an AI agent sends a single JS script to `code_read` / `code_exec`, processes files and filters noise locally, and receives only the distilled output + unified diffs.

## Tools Exposed
- `code_read` — **Strictly read-only** JS sandbox (`fs.glob`, `fs.stat`, `fs.read`, `fs.readLines`, `fs.symbols`, `fs.readFn`, `fs.readFnAt`, `fs.grep`, `fs.grepFn`, `out.*`, `git.status`, `git.diff`) with zero write or shell access—safe to auto-approve independently. Also accepts `{ outputId, outputOffset }` to paginate through cached truncated outputs in ~1 ms without re-running scripts.
- `code_exec` — Execute JS inside a workspace-jailed Goja VM with **cross-file atomic staging** (rolls back all staged writes automatically if any edit throws), plus:
  - `print(...)` / `console.log(...)` (plain-text output, UTF-8/newline-safe cap, default 9,200 bytes; preserves **head + tail** on truncation and returns a cached `outputId` cursor)
  - `out.limit()` / `out.used()` / `out.remaining()` / `out.truncated()` (budget-aware scripting)
  - `fs.glob(pattern?, optsOrMaxFiles?)` (supports `**`, `{a,b}`, `{ maxFiles, ignore }`, and `.truncated` flag)
  - `fs.stat(path)`
  - `fs.read(path, startLine?, endLine?)` / `fs.readLines(path, startLine?, endLine?)`
  - `fs.symbols(path)` (lists classes/functions/methods/types with 1-based `line..endLine` ranges; handles multi-line block comments, template/raw strings, JS regex literals, TS modifiers/return types, multi-line params, and Go/Rust generics)
  - `fs.readFn(path, symbol, maxLines?)` (extracts full function/class body with 1-based line numbers, ±2 context lines, duplicate-definition `matches`, and closest-symbol `suggestions` on miss)
  - `fs.readFnAt(path, line, maxLines?)` (extracts innermost enclosing function/class around a 1-based line with ±15-line fallback on top-level lines)
  - `fs.grep(regex, globPattern?, optsOrMaxHits?)` (accepts string or JS `/RegExp/i`, `{ maxHits, ignore }`, `.truncated` / `.skippedLargeFiles`, validates swapped arguments)
  - `fs.grepFn(regex, globPattern?, optsOrMaxFns?)` (one-liner grep + deduplicated enclosing functions with `hitLines`)
  - `fs.edit(path, target, replacement, opts?)` / `fs.edit(path, [{ target, replacement }], opts?)` (strict unique match by default; `{ optional, replaceAll, wholeWord, expectCount, maxReplacements }`, atomic multi-edit, consolidated net-per-file Myers diff with `diffMode: "auto" | "stat" | "compact" | "full" | "none"`, and `dryRun`)
  - `fs.editLines(path, startLine, endLine, replacement, opts?)` (1-indexed inclusive line-range replacement with `{ expectStart, expectEnd }` guards)
  - `fs.write(path, content)` / `fs.commit()` / `fs.revert()` (restores all touched files back to pre-script disk state even after `sh.run()` auto-commit)
  - `git.status()` / `git.diff(statOnly?)`
  - `sh.run(command)` & `sh.check(command, optsOrFilterRegex?)` (auto-commits staged edits first; `sh.check` returns `{ ok, code, timedOut, totalLines, matchCount, matches, tail }`; disable via `allowShell: false`)
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
