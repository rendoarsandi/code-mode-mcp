package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

const (
	defaultTimeoutMs  = 15000
	maxTimeoutMs      = 120000
	defaultMaxOutByte = 7500
	maxOutByteLimit   = 65536
)

var defaultSkipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"dist":         true,
	"build":        true,
	".next":        true,
	"coverage":     true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
	"target":       true,
	".wrangler":    true,
	".cache":       true,
}

type ExecRequest struct {
	Code       string `json:"code"`
	Cwd        string `json:"cwd"`
	DryRun     bool   `json:"dryRun"`
	DiffMode   string `json:"diffMode"`
	AllowShell *bool  `json:"allowShell"`
	TimeoutMs  int    `json:"timeoutMs"`
	MaxOutByte int    `json:"maxOutputBytes"`
}

type ExecResult struct {
	Stdout      string   `json:"stdout"`
	Truncated   bool     `json:"truncated,omitempty"`
	Result      any      `json:"result,omitempty"`
	DryRun      bool     `json:"dryRun,omitempty"`
	EditedFiles []string `json:"editedFiles,omitempty"`
	Diffs       []string `json:"diffs,omitempty"`
	DurationMs  int64    `json:"durationMs"`
	Error       string   `json:"error,omitempty"`
}

type boundedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) WriteString(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() >= b.limit {
		b.truncated = true
		return
	}
	remain := b.limit - b.buf.Len()
	if len(s) > remain {
		b.buf.WriteString(s[:remain])
		b.truncated = true
	} else {
		b.buf.WriteString(s)
	}
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type sandbox struct {
	root       string
	dryRun     bool
	diffMode   string
	allowShell bool
	timeout    time.Duration
	out        *boundedBuffer
	editedSet  map[string]bool
	editedList []string
	diffs      []string
	ctx        context.Context
}

func normalizeDiffMode(raw string, dryRun bool) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "none", "stat", "compact", "full":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		if dryRun {
			return "compact"
		}
		return "stat"
	}
}

func (s *sandbox) formatDiff(rel, oldText, newText string) string {
	switch s.diffMode {
	case "none":
		return ""
	case "stat":
		return makeDiffStat(rel, oldText, newText)
	case "compact":
		return makeSimpleDiff(rel, oldText, newText, 12)
	default: // "full"
		return makeSimpleDiff(rel, oldText, newText, 60)
	}
}

func (s *sandbox) resolvePath(rel string) (string, string, error) {
	if rel == "" {
		rel = "."
	}
	var target string
	if filepath.IsAbs(rel) {
		target = filepath.Clean(rel)
	} else {
		target = filepath.Clean(filepath.Join(s.root, rel))
	}
	// Resolve symlinks on root and nearest existing parent to prevent symlink escape
	realRoot, err := filepath.EvalSymlinks(s.root)
	if err != nil {
		realRoot = s.root
	}
	checkPath := target
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		checkPath = resolved
	} else if resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(target)); err == nil {
		checkPath = filepath.Join(resolvedParent, filepath.Base(target))
	}

	relOut, err := filepath.Rel(realRoot, checkPath)
	if err != nil || relOut == ".." || strings.HasPrefix(relOut, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path %q escapes workspace root %q", rel, s.root)
	}
	return checkPath, filepath.ToSlash(relOut), nil
}

// matchGlob supports globbing with "**" recursive segments and "{a,b,c}" brace alternation.
func matchGlob(pattern, relPath string) bool {
	pattern = filepath.ToSlash(strings.TrimPrefix(pattern, "./"))
	relPath = filepath.ToSlash(strings.TrimPrefix(relPath, "./"))
	if pattern == "" || pattern == "*" || pattern == "**" || pattern == "**/*" {
		return true
	}
	var re strings.Builder
	re.WriteString("^")
	braceDepth := 0
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					re.WriteString("(?:.*/)?")
					i += 2
				} else {
					re.WriteString(".*")
					i++
				}
			} else {
				re.WriteString("[^/]*")
			}
		case '?':
			re.WriteString("[^/]")
		case '{':
			braceDepth++
			re.WriteString("(?:")
		case '}':
			if braceDepth > 0 {
				braceDepth--
				re.WriteByte(')')
			} else {
				re.WriteString("\\}")
			}
		case ',':
			if braceDepth > 0 {
				re.WriteByte('|')
			} else {
				re.WriteByte(',')
			}
		case '.', '+', '(', ')', '|', '^', '$', '[', ']', '\\':
			re.WriteByte('\\')
			re.WriteByte(c)
		default:
			re.WriteByte(c)
		}
	}
	for braceDepth > 0 {
		re.WriteByte(')')
		braceDepth--
	}
	re.WriteString("$")
	matched, err := regexp.MatchString(re.String(), relPath)
	return err == nil && matched
}

func (s *sandbox) walkFiles(pattern string, maxFiles int) ([]string, error) {
	if maxFiles <= 0 {
		maxFiles = 2000
	}
	var out []string
	err := filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		if d.IsDir() {
			name := d.Name()
			if p != s.root && defaultSkipDirs[name] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if matchGlob(pattern, relSlash) {
			out = append(out, relSlash)
			if len(out) >= maxFiles {
				return filepath.SkipAll
			}
		}
		return nil
	})
	return out, err
}

func (s *sandbox) runGit(args ...string) (string, error) {
	cmd := exec.CommandContext(s.ctx, "git", args...)
	cmd.Dir = s.root
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), errors.New(msg)
	}
	return stdout.String(), nil
}

func formatJSVal(v goja.Value) string {
	if v == nil || goja.IsUndefined(v) {
		return "undefined"
	}
	if goja.IsNull(v) {
		return "null"
	}
	exported := v.Export()
	switch val := exported.(type) {
	case string:
		return val
	case int64, float64, bool:
		return fmt.Sprint(val)
	default:
		b, err := json.Marshal(val)
		if err != nil {
			return v.String()
		}
		return string(b)
	}
}

func runCodeExec(req ExecRequest) ExecResult {
	start := time.Now()
	if strings.TrimSpace(req.Code) == "" {
		return ExecResult{Error: "code is required", DurationMs: 0}
	}

	root := req.Cwd
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return ExecResult{Error: fmt.Sprintf("invalid cwd: %v", err)}
	}
	if st, err := os.Stat(absRoot); err != nil || !st.IsDir() {
		return ExecResult{Error: fmt.Sprintf("cwd does not exist or is not a directory: %s", absRoot)}
	}

	timeoutMs := req.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = defaultTimeoutMs
	}
	if timeoutMs > maxTimeoutMs {
		timeoutMs = maxTimeoutMs
	}
	maxOut := req.MaxOutByte
	if maxOut <= 0 {
		maxOut = defaultMaxOutByte
	}
	if maxOut > maxOutByteLimit {
		maxOut = maxOutByteLimit
	}
	allowShell := true
	if req.AllowShell != nil {
		allowShell = *req.AllowShell
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	sb := &sandbox{
		root:       absRoot,
		dryRun:     req.DryRun,
		diffMode:   normalizeDiffMode(req.DiffMode, req.DryRun),
		allowShell: allowShell,
		timeout:    time.Duration(timeoutMs) * time.Millisecond,
		out:        &boundedBuffer{limit: maxOut},
		editedSet:  make(map[string]bool),
		ctx:        ctx,
	}

	vm := goja.New()
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))

	timer := time.AfterFunc(sb.timeout, func() {
		vm.Interrupt("execution timeout exceeded")
	})
	defer timer.Stop()

	// print / console.log
	printFn := func(call goja.FunctionCall) goja.Value {
		parts := make([]string, len(call.Arguments))
		for i, a := range call.Arguments {
			parts[i] = formatJSVal(a)
		}
		sb.out.WriteString(strings.Join(parts, " ") + "\n")
		return goja.Undefined()
	}
	_ = vm.Set("print", printFn)
	_ = vm.Set("console", map[string]any{
		"log":   printFn,
		"info":  printFn,
		"warn":  printFn,
		"error": printFn,
	})

	// fs API
	fsObj := map[string]any{
		"glob": func(call goja.FunctionCall) goja.Value {
			pat := "**/*"
			if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) {
				pat = call.Arguments[0].String()
			}
			maxFiles := 2000
			if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) {
				if n := int(call.Arguments[1].ToInteger()); n > 0 {
					maxFiles = n
				}
			}
			files, err := sb.walkFiles(pat, maxFiles)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			return vm.ToValue(files)
		},
		"stat": func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 1 {
				panic(vm.NewTypeError("fs.stat(path) requires path"))
			}
			abs, rel, err := sb.resolvePath(call.Arguments[0].String())
			if err != nil {
				panic(vm.NewGoError(err))
			}
			info, err := os.Stat(abs)
			if err != nil {
				return vm.ToValue(map[string]any{"exists": false, "path": rel})
			}
			res := map[string]any{
				"exists": true,
				"path":   rel,
				"isDir":  info.IsDir(),
				"size":   info.Size(),
			}
			if !info.IsDir() && info.Size() <= 2*1024*1024 {
				if data, err := os.ReadFile(abs); err == nil {
					res["lines"] = bytes.Count(data, []byte("\n")) + 1
				}
			}
			return vm.ToValue(res)
		},
		"read": func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 1 {
				panic(vm.NewTypeError("fs.read(path, startLine?, endLine?) requires path"))
			}
			abs, _, err := sb.resolvePath(call.Arguments[0].String())
			if err != nil {
				panic(vm.NewGoError(err))
			}
			data, err := os.ReadFile(abs)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			text := string(data)
			if len(call.Arguments) == 1 || (goja.IsUndefined(call.Arguments[1]) && (len(call.Arguments) < 3 || goja.IsUndefined(call.Arguments[2]))) {
				return vm.ToValue(text)
			}
			lines := strings.Split(text, "\n")
			startLine := 1
			if len(call.Arguments) >= 2 && !goja.IsUndefined(call.Arguments[1]) {
				startLine = int(call.Arguments[1].ToInteger())
			}
			endLine := len(lines)
			if len(call.Arguments) >= 3 && !goja.IsUndefined(call.Arguments[2]) {
				endLine = int(call.Arguments[2].ToInteger())
			}
			if startLine < 1 {
				startLine = 1
			}
			if endLine > len(lines) {
				endLine = len(lines)
			}
			if startLine > endLine {
				return vm.ToValue("")
			}
			return vm.ToValue(strings.Join(lines[startLine-1:endLine], "\n"))
		},
		"grep": func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 1 {
				panic(vm.NewTypeError("fs.grep(regex, globPattern?, maxHits?) requires regex"))
			}
			re, err := regexp.Compile(call.Arguments[0].String())
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("invalid regex: %w", err)))
			}
			globPat := "**/*"
			if len(call.Arguments) >= 2 && !goja.IsUndefined(call.Arguments[1]) && call.Arguments[1].String() != "" {
				globPat = call.Arguments[1].String()
			}
			maxHits := 200
			if len(call.Arguments) >= 3 && !goja.IsUndefined(call.Arguments[2]) {
				if n := int(call.Arguments[2].ToInteger()); n > 0 {
					maxHits = n
				}
			}
			files, err := sb.walkFiles(globPat, 2000)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			type hit struct {
				File string `json:"file"`
				Line int    `json:"line"`
				Text string `json:"text"`
			}
			hits := make([]hit, 0, 32)
			for _, rel := range files {
				if sb.ctx.Err() != nil {
					break
				}
				abs := filepath.Join(sb.root, filepath.FromSlash(rel))
				info, err := os.Stat(abs)
				if err != nil || info.IsDir() || info.Size() > 2*1024*1024 {
					continue
				}
				data, err := os.ReadFile(abs)
				if err != nil || bytes.IndexByte(data, 0) != -1 {
					continue // skip binary files
				}
				lines := strings.Split(string(data), "\n")
				for idx, line := range lines {
					if re.MatchString(line) {
						trimmed := strings.TrimSpace(line)
						if len(trimmed) > 200 {
							trimmed = trimmed[:200] + "..."
						}
						hits = append(hits, hit{File: rel, Line: idx + 1, Text: trimmed})
						if len(hits) >= maxHits {
							return vm.ToValue(hits)
						}
					}
				}
			}
			return vm.ToValue(hits)
		},
		"edit": func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 3 {
				panic(vm.NewTypeError("fs.edit(path, target, replacement) requires 3 arguments"))
			}
			abs, rel, err := sb.resolvePath(call.Arguments[0].String())
			if err != nil {
				panic(vm.NewGoError(err))
			}
			target := call.Arguments[1].String()
			replacement := call.Arguments[2].String()
			if target == "" {
				panic(vm.NewTypeError("fs.edit target cannot be empty"))
			}
			data, err := os.ReadFile(abs)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			oldText := string(data)
			count := strings.Count(oldText, target)
			if count == 0 {
				return vm.ToValue(map[string]any{"changed": false, "occurrences": 0, "file": rel})
			}
			newText := strings.ReplaceAll(oldText, target, replacement)
			diff := sb.formatDiff(rel, oldText, newText)
			if !sb.dryRun {
				if err := os.WriteFile(abs, []byte(newText), 0644); err != nil {
					panic(vm.NewGoError(err))
				}
			}
			if !sb.editedSet[rel] {
				sb.editedSet[rel] = true
				sb.editedList = append(sb.editedList, rel)
			}
			if diff != "" && len(sb.diffs) < 20 {
				sb.diffs = append(sb.diffs, diff)
			}
			out := map[string]any{"changed": true, "occurrences": count, "file": rel, "dryRun": sb.dryRun}
			if diff != "" {
				out["diff"] = diff
			}
			return vm.ToValue(out)
		},
		"write": func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 2 {
				panic(vm.NewTypeError("fs.write(path, content) requires 2 arguments"))
			}
			abs, rel, err := sb.resolvePath(call.Arguments[0].String())
			if err != nil {
				panic(vm.NewGoError(err))
			}
			content := call.Arguments[1].String()
			oldBytes, _ := os.ReadFile(abs)
			diff := sb.formatDiff(rel, string(oldBytes), content)
			if !sb.dryRun {
				if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
					panic(vm.NewGoError(err))
				}
				if err := os.WriteFile(abs, []byte(content), 0644); err != nil {
					panic(vm.NewGoError(err))
				}
			}
			if !sb.editedSet[rel] {
				sb.editedSet[rel] = true
				sb.editedList = append(sb.editedList, rel)
			}
			if diff != "" && len(sb.diffs) < 20 {
				sb.diffs = append(sb.diffs, diff)
			}
			return vm.ToValue(map[string]any{"written": !sb.dryRun, "dryRun": sb.dryRun, "file": rel, "bytes": len(content)})
		},
	}
	_ = vm.Set("fs", fsObj)

	// git API
	_ = vm.Set("git", map[string]any{
		"status": func(call goja.FunctionCall) goja.Value {
			out, err := sb.runGit("status", "--short")
			if err != nil {
				panic(vm.NewGoError(err))
			}
			return vm.ToValue(strings.TrimSpace(out))
		},
		"diff": func(call goja.FunctionCall) goja.Value {
			statOnly := false
			if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) {
				statOnly = call.Arguments[0].ToBoolean()
			}
			args := []string{"diff"}
			if statOnly {
				args = append(args, "--stat")
			}
			out, err := sb.runGit(args...)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			return vm.ToValue(strings.TrimSpace(out))
		},
	})

	// sh API
	_ = vm.Set("sh", map[string]any{
		"run": func(call goja.FunctionCall) goja.Value {
			if !sb.allowShell {
				panic(vm.NewGoError(errors.New("sh.run is disabled (allowShell=false)")))
			}
			if len(call.Arguments) < 1 {
				panic(vm.NewTypeError("sh.run(command) requires command"))
			}
			cmdStr := call.Arguments[0].String()
			cmd := exec.CommandContext(sb.ctx, "sh", "-c", cmdStr)
			cmd.Dir = sb.root
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			code := 0
			if err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					code = exitErr.ExitCode()
				} else {
					code = -1
				}
			}
			return vm.ToValue(map[string]any{
				"code":   code,
				"stdout": stdout.String(),
				"stderr": stderr.String(),
			})
		},
	})

	val, runErr := vm.RunString(req.Code)
	res := ExecResult{
		Stdout:      sb.out.String(),
		Truncated:   sb.out.truncated,
		DryRun:      sb.dryRun,
		EditedFiles: sb.editedList,
		Diffs:       sb.diffs,
		DurationMs:  time.Since(start).Milliseconds(),
	}
	if runErr != nil {
		res.Error = runErr.Error()
		return res
	}
	if val != nil && !goja.IsUndefined(val) && !goja.IsNull(val) {
		res.Result = val.Export()
	}
	return res
}
