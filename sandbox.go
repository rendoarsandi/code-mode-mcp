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
	ReadOnly   bool   `json:"readOnly"`
	Atomic     *bool  `json:"atomic"`
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
	RolledBack  bool     `json:"rolledBack,omitempty"`
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
	root        string
	dryRun      bool
	readOnly    bool
	atomic      bool
	diffMode    string
	diffBytes   int
	allowShell  bool
	timeout     time.Duration
	out         *boundedBuffer
	editedSet   map[string]bool
	editedList  []string
	diffs       []string
	staged      map[string][]byte
	stagedOrder []string
	ctx         context.Context
}

func (s *sandbox) readFileBytes(abs string) ([]byte, error) {
	if data, ok := s.staged[abs]; ok {
		return data, nil
	}
	return os.ReadFile(abs)
}

func (s *sandbox) stageWrite(abs string, data []byte) error {
	if s.readOnly {
		return errors.New("write operations are disabled in read-only mode (code_read)")
	}
	if s.dryRun || s.atomic {
		if _, exists := s.staged[abs]; !exists {
			s.stagedOrder = append(s.stagedOrder, abs)
		}
		cp := make([]byte, len(data))
		copy(cp, data)
		s.staged[abs] = cp
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		return err
	}
	return os.WriteFile(abs, data, 0644)
}

func (s *sandbox) commitStaged() error {
	if s.dryRun || len(s.stagedOrder) == 0 {
		return nil
	}
	for _, abs := range s.stagedOrder {
		data := s.staged[abs]
		if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, data, 0644); err != nil {
			return err
		}
	}
	s.staged = make(map[string][]byte)
	s.stagedOrder = nil
	return nil
}

func normalizeDiffMode(raw string, dryRun bool) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "none", "stat", "compact", "full", "auto":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		if dryRun {
			return "compact"
		}
		return "auto"
	}
}

func (s *sandbox) formatDiff(rel, oldText, newText string) string {
	switch s.diffMode {
	case "none":
		return ""
	case "stat":
		return makeDiffStat(rel, oldText, newText)
	case "compact":
		return makeSimpleDiff(rel, oldText, newText, 20)
	case "full":
		return makeSimpleDiff(rel, oldText, newText, 60)
	default: // "auto": compact ±3-line diff for small edits while within inline budget, else stat
		added, removed := diffStats(oldText, newText)
		if added+removed <= 25 && s.diffBytes < 2600 {
			d := makeSimpleDiff(rel, oldText, newText, 20)
			if s.diffBytes+len(d) <= 3200 {
				s.diffBytes += len(d)
				return d
			}
		}
		st := makeDiffStat(rel, oldText, newText)
		s.diffBytes += len(st)
		return st
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
	allowShell := !req.ReadOnly
	if req.AllowShell != nil && !req.ReadOnly {
		allowShell = *req.AllowShell
	}
	atomic := true
	if req.Atomic != nil {
		atomic = *req.Atomic
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	sb := &sandbox{
		root:       absRoot,
		dryRun:     req.DryRun,
		readOnly:   req.ReadOnly,
		atomic:     atomic,
		diffMode:   normalizeDiffMode(req.DiffMode, req.DryRun),
		allowShell: allowShell,
		timeout:    time.Duration(timeoutMs) * time.Millisecond,
		out:        &boundedBuffer{limit: maxOut},
		editedSet:  make(map[string]bool),
		staged:     make(map[string][]byte),
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
			if stagedData, ok := sb.staged[abs]; ok {
				return vm.ToValue(map[string]any{
					"exists": true,
					"path":   rel,
					"isDir":  false,
					"size":   int64(len(stagedData)),
					"lines":  bytes.Count(stagedData, []byte("\n")) + 1,
				})
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
				if data, err := sb.readFileBytes(abs); err == nil {
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
			data, err := sb.readFileBytes(abs)
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
		"readLines": func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 1 {
				panic(vm.NewTypeError("fs.readLines(path, startLine?, endLine?) requires path"))
			}
			abs, _, err := sb.resolvePath(call.Arguments[0].String())
			if err != nil {
				panic(vm.NewGoError(err))
			}
			data, err := sb.readFileBytes(abs)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			lines := strings.Split(string(data), "\n")
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
			return vm.ToValue(formatNumberedLines(lines[startLine-1:endLine], startLine))
		},
		"readFn": func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 2 {
				panic(vm.NewTypeError("fs.readFn(path, symbol, maxLines?) requires path and symbol"))
			}
			abs, rel, err := sb.resolvePath(call.Arguments[0].String())
			if err != nil {
				panic(vm.NewGoError(err))
			}
			symbol := strings.TrimSpace(call.Arguments[1].String())
			if symbol == "" {
				panic(vm.NewTypeError("fs.readFn symbol cannot be empty"))
			}
			maxLines := 200
			if len(call.Arguments) >= 3 && !goja.IsUndefined(call.Arguments[2]) {
				if n := int(call.Arguments[2].ToInteger()); n > 0 {
					maxLines = n
				}
			}
			data, err := sb.readFileBytes(abs)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			return vm.ToValue(extractFunction(rel, string(data), symbol, maxLines))
		},
		"grep": func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 1 || goja.IsUndefined(call.Arguments[0]) || goja.IsNull(call.Arguments[0]) {
				panic(vm.NewTypeError("fs.grep(regex, globPattern?, maxHits?) requires a string or RegExp as argument 1"))
			}
			var rawPattern string
			arg0 := call.Arguments[0]
			if obj, ok := arg0.(*goja.Object); ok {
				if obj.ClassName() == "RegExp" {
					src := obj.Get("source").String()
					flags := ""
					if fv := obj.Get("flags"); fv != nil && !goja.IsUndefined(fv) {
						flags = fv.String()
					}
					prefix := ""
					if strings.Contains(flags, "i") {
						prefix += "i"
					}
					if strings.Contains(flags, "m") {
						prefix += "m"
					}
					if prefix != "" {
						rawPattern = "(?" + prefix + ")" + src
					} else {
						rawPattern = src
					}
				} else {
					panic(vm.NewTypeError("fs.grep(regex, globPattern?, maxHits?): first argument must be a string or RegExp"))
				}
			} else if _, ok := arg0.Export().(string); ok {
				rawPattern = arg0.String()
			} else {
				panic(vm.NewTypeError("fs.grep(regex, globPattern?, maxHits?): first argument must be a string or RegExp"))
			}

			re, err := regexp.Compile(rawPattern)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fs.grep invalid regex %q: %w", rawPattern, err)))
			}

			globPat := "**/*"
			hasGlobArg := false
			if len(call.Arguments) >= 2 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
				if _, ok := call.Arguments[1].Export().(string); !ok {
					panic(vm.NewTypeError("fs.grep(regex, globPattern?, maxHits?): second argument globPattern must be a string"))
				}
				if s := call.Arguments[1].String(); s != "" {
					globPat = s
					hasGlobArg = true
				}
			}
			maxHits := 200
			if len(call.Arguments) >= 3 && !goja.IsUndefined(call.Arguments[2]) && !goja.IsNull(call.Arguments[2]) {
				if n := int(call.Arguments[2].ToInteger()); n > 0 {
					maxHits = n
				}
			}
			files, err := sb.walkFiles(globPat, 2000)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			// Catch swapped (filePath, regex) arguments instead of silently returning [].
			if hasGlobArg && len(files) == 0 {
				if abs0, _, rerr := sb.resolvePath(rawPattern); rerr == nil {
					if _, serr := os.Stat(abs0); serr == nil {
						panic(vm.NewTypeError(fmt.Sprintf("fs.grep: globPattern %q matched 0 files, while first arg %q is an existing path — arguments look swapped; signature is fs.grep(regex, globPattern?, maxHits?)", globPat, rawPattern)))
					}
				}
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
				data, err := sb.readFileBytes(abs)
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
			if len(call.Arguments) < 2 {
				panic(vm.NewTypeError("fs.edit(path, target, replacement, opts?) or fs.edit(path, [{target, replacement}], opts?)"))
			}
			abs, rel, err := sb.resolvePath(call.Arguments[0].String())
			if err != nil {
				panic(vm.NewGoError(err))
			}

			parseOpts := func(v goja.Value, defOpt, defAll bool) (bool, bool) {
				optional, replaceAll := defOpt, defAll
				if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
					return optional, replaceAll
				}
				obj := v.ToObject(vm)
				if obj == nil {
					return optional, replaceAll
				}
				if ov := obj.Get("optional"); ov != nil && !goja.IsUndefined(ov) {
					optional = ov.ToBoolean()
				}
				if rv := obj.Get("replaceAll"); rv != nil && !goja.IsUndefined(rv) {
					replaceAll = rv.ToBoolean()
				}
				return optional, replaceAll
			}

			type editSpec struct {
				target      string
				replacement string
				optional    bool
				replaceAll  bool
			}

			isMulti := false
			var specs []editSpec

			arg1 := call.Arguments[1]
			if arg1 != nil && !goja.IsUndefined(arg1) && !goja.IsNull(arg1) {
				if obj, ok := arg1.(*goja.Object); ok && obj.ClassName() == "Array" {
					isMulti = true
					var topOpts goja.Value
					if len(call.Arguments) >= 3 {
						topOpts = call.Arguments[2]
					}
					defOpt, defAll := parseOpts(topOpts, false, false)
					n := int(obj.Get("length").ToInteger())
					if n == 0 {
						panic(vm.NewTypeError("fs.edit multi-edit array cannot be empty"))
					}
					specs = make([]editSpec, 0, n)
					for i := 0; i < n; i++ {
						itemVal := obj.Get(fmt.Sprintf("%d", i))
						if itemVal == nil || goja.IsUndefined(itemVal) || goja.IsNull(itemVal) {
							panic(vm.NewTypeError(fmt.Sprintf("fs.edit edit[%d] must be an object", i)))
						}
						itemObj := itemVal.ToObject(vm)
						tVal := itemObj.Get("target")
						if tVal == nil || goja.IsUndefined(tVal) {
							tVal = itemObj.Get("old")
						}
						rVal := itemObj.Get("replacement")
						if rVal == nil || goja.IsUndefined(rVal) {
							rVal = itemObj.Get("new")
						}
						if tVal == nil || goja.IsUndefined(tVal) || rVal == nil || goja.IsUndefined(rVal) {
							panic(vm.NewTypeError(fmt.Sprintf("fs.edit edit[%d] requires target and replacement", i)))
						}
						target := tVal.String()
						if target == "" {
							panic(vm.NewTypeError(fmt.Sprintf("fs.edit edit[%d] target cannot be empty", i)))
						}
						iOpt, iAll := parseOpts(itemVal, defOpt, defAll)
						specs = append(specs, editSpec{
							target:      target,
							replacement: rVal.String(),
							optional:    iOpt,
							replaceAll:  iAll,
						})
					}
				}
			}

			if !isMulti {
				if len(call.Arguments) < 3 {
					panic(vm.NewTypeError("fs.edit(path, target, replacement, opts?) requires at least 3 arguments"))
				}
				target := call.Arguments[1].String()
				replacement := call.Arguments[2].String()
				if target == "" {
					panic(vm.NewTypeError("fs.edit target cannot be empty"))
				}
				var optsVal goja.Value
				if len(call.Arguments) >= 4 {
					optsVal = call.Arguments[3]
				}
				opt, repAll := parseOpts(optsVal, false, false)
				specs = []editSpec{{target: target, replacement: replacement, optional: opt, replaceAll: repAll}}
			}

			data, err := sb.readFileBytes(abs)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			oldText := string(data)
			curText := oldText
			totalOcc := 0
			editsApplied := 0

			previewTarget := func(s string) string {
				clean := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\\n"))
				r := []rune(clean)
				if len(r) > 40 {
					return string(r[:40]) + "..."
				}
				return clean
			}

			for idx, sp := range specs {
				count := strings.Count(curText, sp.target)
				if count == 0 {
					if sp.optional {
						continue
					}
					prev := previewTarget(sp.target)
					if isMulti {
						panic(vm.NewGoError(fmt.Errorf("fs.edit: edit[%d] target %q not found in %s (pass { optional: true } to ignore)", idx, prev, rel)))
					}
					panic(vm.NewGoError(fmt.Errorf("fs.edit: target %q not found in %s (pass { optional: true } to ignore)", prev, rel)))
				}
				if count > 1 && !sp.replaceAll {
					prev := previewTarget(sp.target)
					if isMulti {
						panic(vm.NewGoError(fmt.Errorf("fs.edit: edit[%d] target %q matched %d times in %s (pass { replaceAll: true } or add surrounding context)", idx, prev, count, rel)))
					}
					panic(vm.NewGoError(fmt.Errorf("fs.edit: target %q matched %d times in %s (pass { replaceAll: true } or add surrounding context)", prev, count, rel)))
				}
				if sp.replaceAll {
					curText = strings.ReplaceAll(curText, sp.target, sp.replacement)
					totalOcc += count
				} else {
					curText = strings.Replace(curText, sp.target, sp.replacement, 1)
					totalOcc += 1
				}
				editsApplied++
			}

			if curText == oldText {
				out := map[string]any{"changed": false, "occurrences": 0, "file": rel, "dryRun": sb.dryRun}
				if isMulti {
					out["editsApplied"] = 0
				}
				return vm.ToValue(out)
			}

			diff := sb.formatDiff(rel, oldText, curText)
			if err := sb.stageWrite(abs, []byte(curText)); err != nil {
				panic(vm.NewGoError(err))
			}
			if !sb.editedSet[rel] {
				sb.editedSet[rel] = true
				sb.editedList = append(sb.editedList, rel)
			}
			if diff != "" && len(sb.diffs) < 20 {
				sb.diffs = append(sb.diffs, diff)
			}
			out := map[string]any{"changed": true, "occurrences": totalOcc, "file": rel, "dryRun": sb.dryRun}
			if isMulti {
				out["editsApplied"] = editsApplied
			}
			if diff != "" {
				out["diff"] = diff
			}
			return vm.ToValue(out)
		},
		"editLines": func(call goja.FunctionCall) goja.Value {
			if sb.readOnly {
				panic(vm.NewGoError(errors.New("fs.editLines is disabled in read-only mode (code_read)")))
			}
			if len(call.Arguments) < 4 {
				panic(vm.NewTypeError("fs.editLines(path, startLine, endLine, replacement, opts?) requires 4 arguments"))
			}
			abs, rel, err := sb.resolvePath(call.Arguments[0].String())
			if err != nil {
				panic(vm.NewGoError(err))
			}
			startLine := int(call.Arguments[1].ToInteger())
			endLine := int(call.Arguments[2].ToInteger())
			replacement := call.Arguments[3].String()

			expectStart := ""
			if len(call.Arguments) >= 5 && !goja.IsUndefined(call.Arguments[4]) && !goja.IsNull(call.Arguments[4]) {
				if obj := call.Arguments[4].ToObject(vm); obj != nil {
					if ev := obj.Get("expectStart"); ev != nil && !goja.IsUndefined(ev) && !goja.IsNull(ev) {
						expectStart = ev.String()
					}
				}
			}

			data, err := sb.readFileBytes(abs)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			oldText := string(data)
			lines := strings.Split(oldText, "\n")
			if startLine < 1 || endLine < startLine || endLine > len(lines) {
				panic(vm.NewGoError(fmt.Errorf("fs.editLines: invalid line range %d..%d for %s (%d lines)", startLine, endLine, rel, len(lines))))
			}
			if expectStart != "" && !strings.Contains(lines[startLine-1], expectStart) {
				panic(vm.NewGoError(fmt.Errorf("fs.editLines: line %d in %s does not contain expectStart %q (got %q)", startLine, rel, expectStart, strings.TrimSpace(lines[startLine-1]))))
			}

			var repLines []string
			if replacement != "" {
				repLines = strings.Split(replacement, "\n")
			}
			newLines := make([]string, 0, len(lines)-(endLine-startLine+1)+len(repLines))
			newLines = append(newLines, lines[:startLine-1]...)
			newLines = append(newLines, repLines...)
			newLines = append(newLines, lines[endLine:]...)
			newText := strings.Join(newLines, "\n")

			if newText == oldText {
				return vm.ToValue(map[string]any{"changed": false, "file": rel, "dryRun": sb.dryRun})
			}
			diff := sb.formatDiff(rel, oldText, newText)
			if err := sb.stageWrite(abs, []byte(newText)); err != nil {
				panic(vm.NewGoError(err))
			}
			if !sb.editedSet[rel] {
				sb.editedSet[rel] = true
				sb.editedList = append(sb.editedList, rel)
			}
			if diff != "" && len(sb.diffs) < 20 {
				sb.diffs = append(sb.diffs, diff)
			}
			out := map[string]any{"changed": true, "file": rel, "startLine": startLine, "endLine": endLine, "dryRun": sb.dryRun}
			if diff != "" {
				out["diff"] = diff
			}
			return vm.ToValue(out)
		},
		"write": func(call goja.FunctionCall) goja.Value {
			if sb.readOnly {
				panic(vm.NewGoError(errors.New("fs.write is disabled in read-only mode (code_read)")))
			}
			if len(call.Arguments) < 2 {
				panic(vm.NewTypeError("fs.write(path, content) requires 2 arguments"))
			}
			abs, rel, err := sb.resolvePath(call.Arguments[0].String())
			if err != nil {
				panic(vm.NewGoError(err))
			}
			content := call.Arguments[1].String()
			oldBytes, _ := sb.readFileBytes(abs)
			diff := sb.formatDiff(rel, string(oldBytes), content)
			if err := sb.stageWrite(abs, []byte(content)); err != nil {
				panic(vm.NewGoError(err))
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
		"commit": func(call goja.FunctionCall) goja.Value {
			if sb.readOnly {
				panic(vm.NewGoError(errors.New("fs.commit is disabled in read-only mode (code_read)")))
			}
			count := len(sb.stagedOrder)
			if err := sb.commitStaged(); err != nil {
				panic(vm.NewGoError(err))
			}
			return vm.ToValue(map[string]any{"committed": count, "dryRun": sb.dryRun})
		},
	}
	if sb.readOnly {
		delete(fsObj, "edit")
		delete(fsObj, "editLines")
		delete(fsObj, "write")
		delete(fsObj, "commit")
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
			if sb.readOnly || !sb.allowShell {
				panic(vm.NewGoError(errors.New("sh.run is disabled (readOnly or allowShell=false)")))
			}
			if len(call.Arguments) < 1 {
				panic(vm.NewTypeError("sh.run(command) requires command"))
			}
			// Auto-commit staged edits so tests/builds invoked via sh.run see the updated files.
			if err := sb.commitStaged(); err != nil {
				panic(vm.NewGoError(err))
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
	if runErr != nil {
		rolledBack := sb.atomic && !sb.dryRun && len(sb.stagedOrder) > 0
		res := ExecResult{
			Stdout:     sb.out.String(),
			Truncated:  sb.out.truncated,
			DryRun:     sb.dryRun,
			RolledBack: rolledBack,
			DurationMs: time.Since(start).Milliseconds(),
			Error:      runErr.Error(),
		}
		if !rolledBack {
			res.EditedFiles = sb.editedList
			res.Diffs = sb.diffs
		}
		return res
	}

	if commitErr := sb.commitStaged(); commitErr != nil {
		return ExecResult{
			Stdout:     sb.out.String(),
			Truncated:  sb.out.truncated,
			DryRun:     sb.dryRun,
			DurationMs: time.Since(start).Milliseconds(),
			Error:      "atomic commit failed: " + commitErr.Error(),
		}
	}

	res := ExecResult{
		Stdout:      sb.out.String(),
		Truncated:   sb.out.truncated,
		DryRun:      sb.dryRun,
		EditedFiles: sb.editedList,
		Diffs:       sb.diffs,
		DurationMs:  time.Since(start).Milliseconds(),
	}
	if val != nil && !goja.IsUndefined(val) && !goja.IsNull(val) {
		res.Result = val.Export()
	}
	return res
}
