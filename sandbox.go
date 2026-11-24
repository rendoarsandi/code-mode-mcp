package main

import (
	"bytes"
	"context"
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
	"unicode/utf8"

	"github.com/dop251/goja"
)

const (
	defaultReadTimeoutMs = 15000
	defaultExecTimeoutMs = 45000
	maxTimeoutMs         = 300000
	defaultMaxOutByte    = 9200
	maxOutByteLimit      = 65536
	maxCacheBytes        = 512 * 1024
	maxCachedOutputs     = 8
	maxTailPreserveBytes = 2048
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

var defaultCheckFailRe = regexp.MustCompile(`(?i)(FAIL|ERROR|ERR!|Exception|AssertionError|ReferenceError|TypeError|SyntaxError|panic:|--- FAIL|✗|✘)`)

type ExecRequest struct {
	Code         string `json:"code"`
	Cwd          string `json:"cwd"`
	DryRun       bool   `json:"dryRun"`
	ReadOnly     bool   `json:"readOnly"`
	Atomic       *bool  `json:"atomic"`
	DiffMode     string `json:"diffMode"`
	AllowShell   *bool  `json:"allowShell"`
	TimeoutMs    int    `json:"timeoutMs"`
	MaxOutByte   int    `json:"maxOutputBytes"`
	OutputId     string `json:"outputId"`
	OutputOffset int    `json:"outputOffset"`
}

type ExecResult struct {
	Stdout       string   `json:"stdout"`
	Truncated    bool     `json:"truncated,omitempty"`
	TotalBytes   int      `json:"totalBytes,omitempty"`
	ShownBytes   int      `json:"shownBytes,omitempty"`
	OutputId     string   `json:"outputId,omitempty"`
	OutputOffset int      `json:"outputOffset,omitempty"`
	NextOffset   int      `json:"nextOffset,omitempty"`
	Result       any      `json:"result,omitempty"`
	DryRun       bool     `json:"dryRun,omitempty"`
	RolledBack   bool     `json:"rolledBack,omitempty"`
	EditedFiles  []string `json:"editedFiles,omitempty"`
	Diffs        []string `json:"diffs,omitempty"`
	DurationMs   int64    `json:"durationMs"`
	Error        string   `json:"error,omitempty"`
	maxOut       int
}

type outputCache struct {
	mu    sync.Mutex
	seq   int
	cap   int
	order []string
	items map[string]string
}

func newOutputCache(capacity int) *outputCache {
	return &outputCache{
		cap:   capacity,
		items: make(map[string]string, capacity),
	}
}

func (c *outputCache) put(full string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	id := fmt.Sprintf("out_%d", c.seq)
	c.items[id] = full
	c.order = append(c.order, id)
	for len(c.order) > c.cap {
		evict := c.order[0]
		c.order = c.order[1:]
		delete(c.items, evict)
	}
	return id
}

func (c *outputCache) get(id string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[id]
	return v, ok
}

var globalOutputCache = newOutputCache(maxCachedOutputs)

type boundedBuffer struct {
	mu           sync.Mutex
	full         bytes.Buffer
	tail         []byte
	limit        int
	totalBytes   int
	fullOverflow bool
}

func trimUTF8Prefix(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func trimUTF8Suffix(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

func snapHeadPrefix(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	prefix := trimUTF8Prefix(s, maxBytes)
	if nl := strings.LastIndexByte(prefix, '\n'); nl >= 0 && nl+1 >= maxBytes*3/4 {
		return prefix[:nl+1]
	}
	return prefix
}

func snapTailSuffix(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	suffix := trimUTF8Suffix(s, maxBytes)
	if nl := strings.IndexByte(suffix, '\n'); nl >= 0 && len(suffix)-(nl+1) >= maxBytes*3/5 {
		return suffix[nl+1:]
	}
	return suffix
}

func (b *boundedBuffer) WriteString(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.totalBytes += len(s)
	if !b.fullOverflow {
		remain := maxCacheBytes - b.full.Len()
		if len(s) <= remain {
			b.full.WriteString(s)
		} else {
			if remain > 0 {
				b.full.WriteString(trimUTF8Prefix(s, remain))
			}
			b.fullOverflow = true
		}
	}
	if b.fullOverflow {
		if len(s) >= maxTailPreserveBytes {
			b.tail = append(b.tail[:0], s[len(s)-maxTailPreserveBytes:]...)
		} else {
			b.tail = append(b.tail, s...)
			if len(b.tail) > maxTailPreserveBytes {
				b.tail = append(b.tail[:0], b.tail[len(b.tail)-maxTailPreserveBytes:]...)
			}
		}
	}
}

func (b *boundedBuffer) TotalBytes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.totalBytes
}

func (b *boundedBuffer) Render() (stdout string, truncated bool, totalBytes int, shownBytes int, nextOffset int, fullText string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	totalBytes = b.totalBytes
	fullStr := b.full.String()
	if totalBytes <= b.limit && !b.fullOverflow {
		return fullStr, false, totalBytes, totalBytes, 0, ""
	}
	// Preserve exact head-only truncation for small budgets (<512 bytes) where splitting makes no sense.
	if b.limit < 512 {
		head := snapHeadPrefix(fullStr, b.limit)
		return head, true, totalBytes, len(head), len(head), fullStr
	}

	// Head + tail preservation so trailing summaries/errors are never silently lost.
	tailBudget := b.limit / 5
	if tailBudget < 200 {
		tailBudget = 200
	}
	if tailBudget > 1500 {
		tailBudget = 1500
	}
	const markerReserve = 56
	headBudget := b.limit - tailBudget - markerReserve
	if headBudget < 200 {
		head := snapHeadPrefix(fullStr, b.limit)
		return head, true, totalBytes, len(head), len(head), fullStr
	}

	head := snapHeadPrefix(fullStr, headBudget)
	tailSource := fullStr
	if b.fullOverflow && len(b.tail) > 0 {
		tailSource = string(b.tail)
	}
	tail := snapTailSuffix(tailSource, tailBudget)
	if !b.fullOverflow && len(head)+len(tail) >= len(fullStr) {
		headOnly := snapHeadPrefix(fullStr, b.limit)
		return headOnly, true, totalBytes, len(headOnly), len(headOnly), fullStr
	}

	omitted := totalBytes - len(head) - len(tail)
	if omitted < 0 {
		omitted = 0
	}
	headTrim := strings.TrimRight(head, "\n")
	tailTrim := strings.TrimLeft(tail, "\n")
	combined := fmt.Sprintf("%s\n... [%d bytes omitted] ...\n%s", headTrim, omitted, tailTrim)
	return combined, true, totalBytes, len(head) + len(tail), len(head), fullStr
}

type origFileSnap struct {
	existed bool
	data    []byte
}

type sandbox struct {
	root            string
	dryRun          bool
	readOnly        bool
	atomic          bool
	diffMode        string
	diffBytes       int
	allowShell      bool
	timeout         time.Duration
	out             *boundedBuffer
	originals       map[string]origFileSnap
	originalOrder   []string
	absByRel        map[string]string
	committedOnDisk map[string]bool
	staged          map[string][]byte
	stagedOrder     []string
	ctx             context.Context
}

func (s *sandbox) readFileBytes(abs string) ([]byte, error) {
	if data, ok := s.staged[abs]; ok {
		return data, nil
	}
	return os.ReadFile(abs)
}

func (s *sandbox) recordOriginal(abs, rel string) {
	if _, exists := s.originals[abs]; exists {
		return
	}
	data, err := os.ReadFile(abs)
	if err == nil {
		cp := make([]byte, len(data))
		copy(cp, data)
		s.originals[abs] = origFileSnap{existed: true, data: cp}
	} else {
		s.originals[abs] = origFileSnap{existed: false}
	}
	s.originalOrder = append(s.originalOrder, rel)
	s.absByRel[rel] = abs
}

func (s *sandbox) stageWrite(abs, rel string, data []byte) error {
	if s.readOnly {
		return errors.New("write operations are disabled in read-only mode (code_read)")
	}
	s.recordOriginal(abs, rel)
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
	if err := os.WriteFile(abs, data, 0644); err != nil {
		return err
	}
	s.committedOnDisk[abs] = true
	return nil
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
		s.committedOnDisk[abs] = true
	}
	s.staged = make(map[string][]byte)
	s.stagedOrder = nil
	return nil
}

func (s *sandbox) revertAll() (int, error) {
	reverted := len(s.originalOrder)
	var firstErr error
	if !s.dryRun {
		for _, rel := range s.originalOrder {
			abs := s.absByRel[rel]
			if !s.committedOnDisk[abs] {
				continue
			}
			orig := s.originals[abs]
			if orig.existed {
				if err := os.WriteFile(abs, orig.data, 0644); err != nil && firstErr == nil {
					firstErr = err
				}
			} else {
				if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	s.staged = make(map[string][]byte)
	s.stagedOrder = nil
	s.originals = make(map[string]origFileSnap)
	s.originalOrder = nil
	s.absByRel = make(map[string]string)
	s.committedOnDisk = make(map[string]bool)
	s.diffBytes = 0
	return reverted, firstErr
}

// computeNetDiffs produces one clean net diff per modified file (original disk content vs final content).
func (s *sandbox) computeNetDiffs(committedOnly bool) (edited []string, diffs []string) {
	s.diffBytes = 0
	omittedFiles := 0
	for _, rel := range s.originalOrder {
		abs := s.absByRel[rel]
		if committedOnly && !s.committedOnDisk[abs] {
			continue
		}
		orig := s.originals[abs]
		var curBytes []byte
		if !committedOnly {
			if stData, ok := s.staged[abs]; ok {
				curBytes = stData
			} else {
				curBytes, _ = os.ReadFile(abs)
			}
		} else {
			curBytes, _ = os.ReadFile(abs)
		}
		if orig.existed && bytes.Equal(orig.data, curBytes) {
			continue
		}
		edited = append(edited, rel)
		if len(diffs) < 20 {
			if d := s.formatDiff(rel, string(orig.data), string(curBytes)); d != "" {
				diffs = append(diffs, d)
			}
		} else {
			omittedFiles++
		}
	}
	if omittedFiles > 0 {
		diffs = append(diffs, fmt.Sprintf("... (+%d more edited files omitted from diffs)", omittedFiles))
	}
	return edited, diffs
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

func matchesAnyGlob(patterns []string, relPath string) bool {
	for _, p := range patterns {
		if p != "" && matchGlob(p, relPath) {
			return true
		}
	}
	return false
}

func (s *sandbox) walkFiles(pattern string, maxFiles int, ignore []string) ([]string, bool, error) {
	if maxFiles <= 0 {
		maxFiles = 2000
	}
	var out []string
	truncated := false
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
		if len(ignore) > 0 && matchesAnyGlob(ignore, relSlash) {
			return nil
		}
		if matchGlob(pattern, relSlash) {
			if len(out) >= maxFiles {
				truncated = true
				return filepath.SkipAll
			}
			out = append(out, relSlash)
		}
		return nil
	})
	return out, truncated, err
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

func (s *sandbox) runShellCommand(cmdStr string) (code int, stdout string, stderr string, timedOut bool, err error) {
	if err := s.commitStaged(); err != nil {
		return -1, "", "", false, err
	}
	cmd := exec.CommandContext(s.ctx, "sh", "-c", cmdStr)
	cmd.Dir = s.root
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	code = 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			code = -1
		}
	}
	return code, outBuf.String(), errBuf.String(), errors.Is(s.ctx.Err(), context.DeadlineExceeded), nil
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
		return marshalCompactNoEscape(val)
	}
}

func extractStringSlice(vm *goja.Runtime, v goja.Value) []string {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil
	}
	if obj, ok := v.(*goja.Object); ok && obj.ClassName() == "Array" {
		n := int(obj.Get("length").ToInteger())
		out := make([]string, 0, n)
		for i := 0; i < n; i++ {
			item := obj.Get(fmt.Sprintf("%d", i))
			if item != nil && !goja.IsUndefined(item) && !goja.IsNull(item) {
				out = append(out, item.String())
			}
		}
		return out
	}
	if s, ok := v.Export().(string); ok && s != "" {
		return []string{s}
	}
	return nil
}

func parseRegexArg(vm *goja.Runtime, arg0 goja.Value, fnSig string) (string, *regexp.Regexp) {
	if arg0 == nil || goja.IsUndefined(arg0) || goja.IsNull(arg0) {
		panic(vm.NewTypeError(fnSig + " requires a string or RegExp as argument 1"))
	}
	var rawPattern string
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
			panic(vm.NewTypeError(fnSig + ": first argument must be a string or RegExp"))
		}
	} else if _, ok := arg0.Export().(string); ok {
		rawPattern = arg0.String()
	} else {
		panic(vm.NewTypeError(fnSig + ": first argument must be a string or RegExp"))
	}
	re, err := regexp.Compile(rawPattern)
	if err != nil {
		panic(vm.NewGoError(fmt.Errorf("%s invalid regex %q: %w", fnSig, rawPattern, err)))
	}
	return rawPattern, re
}

func toJSArrayWithProps[T any](vm *goja.Runtime, items []T, props map[string]any) goja.Value {
	arr := vm.NewArray()
	for i, item := range items {
		_ = arr.Set(fmt.Sprintf("%d", i), vm.ToValue(item))
	}
	for k, v := range props {
		_ = arr.DefineDataProperty(k, vm.ToValue(v), goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_FALSE)
	}
	return arr
}

func isIdentRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '$'
}

func findLiteralMatches(text, target string, wholeWord bool) [][2]int {
	if target == "" {
		return nil
	}
	var spans [][2]int
	pos := 0
	for pos <= len(text)-len(target) {
		idx := strings.Index(text[pos:], target)
		if idx < 0 {
			break
		}
		start := pos + idx
		end := start + len(target)
		if wholeWord {
			if start > 0 {
				prevR, _ := utf8.DecodeLastRuneInString(text[:start])
				if isIdentRune(prevR) {
					pos = start + 1
					continue
				}
			}
			if end < len(text) {
				nextR, _ := utf8.DecodeRuneInString(text[end:])
				if isIdentRune(nextR) {
					pos = start + 1
					continue
				}
			}
		}
		spans = append(spans, [2]int{start, end})
		pos = end
	}
	return spans
}

func applySpanReplacements(text string, spans [][2]int, replacement string) string {
	var b strings.Builder
	last := 0
	for _, sp := range spans {
		b.WriteString(text[last:sp[0]])
		b.WriteString(replacement)
		last = sp[1]
	}
	b.WriteString(text[last:])
	return b.String()
}

func serveCachedOutput(req ExecRequest, maxOut int, start time.Time) ExecResult {
	full, ok := globalOutputCache.get(strings.TrimSpace(req.OutputId))
	if !ok {
		return ExecResult{
			Error:      fmt.Sprintf("outputId %q not found or expired (cache holds last %d truncated outputs)", req.OutputId, maxCachedOutputs),
			DurationMs: time.Since(start).Milliseconds(),
			maxOut:     maxOut,
		}
	}
	offset := req.OutputOffset
	if offset < 0 {
		offset = 0
	}
	if offset > len(full) {
		offset = len(full)
	}
	for offset < len(full) && !utf8.RuneStart(full[offset]) {
		offset++
	}
	rest := full[offset:]
	if len(rest) <= maxOut {
		return ExecResult{
			Stdout:       rest,
			Truncated:    false,
			TotalBytes:   len(full),
			ShownBytes:   len(rest),
			OutputId:     req.OutputId,
			OutputOffset: offset,
			DurationMs:   time.Since(start).Milliseconds(),
			maxOut:       maxOut,
		}
	}
	chunk := snapHeadPrefix(rest, maxOut)
	return ExecResult{
		Stdout:       chunk,
		Truncated:    true,
		TotalBytes:   len(full),
		ShownBytes:   len(chunk),
		OutputId:     req.OutputId,
		OutputOffset: offset,
		NextOffset:   offset + len(chunk),
		DurationMs:   time.Since(start).Milliseconds(),
		maxOut:       maxOut,
	}
}

func runCodeExec(req ExecRequest) ExecResult {
	start := time.Now()
	maxOut := req.MaxOutByte
	if maxOut <= 0 {
		maxOut = defaultMaxOutByte
	}
	if maxOut > maxOutByteLimit {
		maxOut = maxOutByteLimit
	}

	// Fast-path continuation read from in-memory output cache without re-executing a script.
	if strings.TrimSpace(req.OutputId) != "" {
		return serveCachedOutput(req, maxOut, start)
	}

	if strings.TrimSpace(req.Code) == "" {
		return ExecResult{Error: "code is required (or pass outputId to read a cached chunk)", DurationMs: 0, maxOut: maxOut}
	}

	root := req.Cwd
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return ExecResult{Error: fmt.Sprintf("invalid cwd: %v", err), maxOut: maxOut}
	}
	if st, err := os.Stat(absRoot); err != nil || !st.IsDir() {
		return ExecResult{Error: fmt.Sprintf("cwd does not exist or is not a directory: %s", absRoot), maxOut: maxOut}
	}

	timeoutMs := req.TimeoutMs
	if timeoutMs <= 0 {
		if req.ReadOnly {
			timeoutMs = defaultReadTimeoutMs
		} else {
			timeoutMs = defaultExecTimeoutMs
		}
	}
	if timeoutMs > maxTimeoutMs {
		timeoutMs = maxTimeoutMs
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
		root:            absRoot,
		dryRun:          req.DryRun,
		readOnly:        req.ReadOnly,
		atomic:          atomic,
		diffMode:        normalizeDiffMode(req.DiffMode, req.DryRun),
		allowShell:      allowShell,
		timeout:         time.Duration(timeoutMs) * time.Millisecond,
		out:             &boundedBuffer{limit: maxOut},
		originals:       make(map[string]origFileSnap),
		absByRel:        make(map[string]string),
		committedOnDisk: make(map[string]bool),
		staged:          make(map[string][]byte),
		ctx:             ctx,
	}

	vm := goja.New()
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))

	timer := time.AfterFunc(sb.timeout, func() {
		vm.Interrupt(fmt.Sprintf("execution timeout exceeded (%dms; pass timeoutMs up to %d for slower builds/tests)", timeoutMs, maxTimeoutMs))
	})
	defer timer.Stop()

	// print / console.log / out budget helper
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
	_ = vm.Set("out", map[string]any{
		"limit": func(call goja.FunctionCall) goja.Value {
			return vm.ToValue(sb.out.limit)
		},
		"used": func(call goja.FunctionCall) goja.Value {
			return vm.ToValue(sb.out.TotalBytes())
		},
		"remaining": func(call goja.FunctionCall) goja.Value {
			rem := sb.out.limit - sb.out.TotalBytes()
			if rem < 0 {
				rem = 0
			}
			return vm.ToValue(rem)
		},
		"truncated": func(call goja.FunctionCall) goja.Value {
			return vm.ToValue(sb.out.TotalBytes() > sb.out.limit)
		},
	})

	// fs API
	fsObj := map[string]any{
		"glob": func(call goja.FunctionCall) goja.Value {
			pat := "**/*"
			if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) && !goja.IsNull(call.Arguments[0]) {
				pat = call.Arguments[0].String()
			}
			maxFiles := 2000
			var ignore []string
			if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
				arg1 := call.Arguments[1]
				if obj, ok := arg1.(*goja.Object); ok {
					if mv := obj.Get("maxFiles"); mv != nil && !goja.IsUndefined(mv) && !goja.IsNull(mv) {
						if n := int(mv.ToInteger()); n > 0 {
							maxFiles = n
						}
					}
					ignore = extractStringSlice(vm, obj.Get("ignore"))
				} else if n := int(arg1.ToInteger()); n > 0 {
					maxFiles = n
				}
			}
			files, truncated, err := sb.walkFiles(pat, maxFiles, ignore)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			return toJSArrayWithProps(vm, files, map[string]any{"truncated": truncated})
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
		"symbols": func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 1 {
				panic(vm.NewTypeError("fs.symbols(path) requires path"))
			}
			abs, rel, err := sb.resolvePath(call.Arguments[0].String())
			if err != nil {
				panic(vm.NewGoError(err))
			}
			data, err := sb.readFileBytes(abs)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			syms := extractSymbols(rel, string(data))
			out := make([]map[string]any, len(syms))
			for i, sym := range syms {
				out[i] = map[string]any{
					"name":    sym.Name,
					"kind":    sym.Kind,
					"line":    sym.Line,
					"endLine": sym.EndLine,
				}
			}
			return vm.ToValue(out)
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
		"readFnAt": func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 2 {
				panic(vm.NewTypeError("fs.readFnAt(path, line, maxLines?) requires path and 1-based line number"))
			}
			abs, rel, err := sb.resolvePath(call.Arguments[0].String())
			if err != nil {
				panic(vm.NewGoError(err))
			}
			lineNum := int(call.Arguments[1].ToInteger())
			if lineNum < 1 {
				panic(vm.NewTypeError("fs.readFnAt line must be a positive 1-based line number"))
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
			return vm.ToValue(extractFunctionAt(rel, string(data), lineNum, maxLines))
		},
		"grep": func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 1 {
				panic(vm.NewTypeError("fs.grep(regex, globPattern?, optsOrMaxHits?) requires a string or RegExp as argument 1"))
			}
			rawPattern, re := parseRegexArg(vm, call.Arguments[0], "fs.grep(regex, globPattern?, optsOrMaxHits?)")

			globPat := "**/*"
			hasGlobArg := false
			if len(call.Arguments) >= 2 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
				if _, ok := call.Arguments[1].Export().(string); !ok {
					panic(vm.NewTypeError("fs.grep(regex, globPattern?, optsOrMaxHits?): second argument globPattern must be a string"))
				}
				if s := call.Arguments[1].String(); s != "" {
					globPat = s
					hasGlobArg = true
				}
			}
			maxHits := 200
			var ignore []string
			if len(call.Arguments) >= 3 && !goja.IsUndefined(call.Arguments[2]) && !goja.IsNull(call.Arguments[2]) {
				arg2 := call.Arguments[2]
				if obj, ok := arg2.(*goja.Object); ok {
					if mv := obj.Get("maxHits"); mv != nil && !goja.IsUndefined(mv) && !goja.IsNull(mv) {
						if n := int(mv.ToInteger()); n > 0 {
							maxHits = n
						}
					}
					ignore = extractStringSlice(vm, obj.Get("ignore"))
				} else if n := int(arg2.ToInteger()); n > 0 {
					maxHits = n
				}
			}
			files, _, err := sb.walkFiles(globPat, 2000, ignore)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			// Catch swapped (filePath, regex) arguments instead of silently returning [].
			if hasGlobArg && len(files) == 0 {
				if abs0, _, rerr := sb.resolvePath(rawPattern); rerr == nil {
					if _, serr := os.Stat(abs0); serr == nil {
						panic(vm.NewTypeError(fmt.Sprintf("fs.grep: globPattern %q matched 0 files, while first arg %q is an existing path — arguments look swapped; signature is fs.grep(regex, globPattern?, optsOrMaxHits?)", globPat, rawPattern)))
					}
				}
			}
			type hit struct {
				File string `json:"file"`
				Line int    `json:"line"`
				Text string `json:"text"`
			}
			hits := make([]hit, 0, 32)
			truncated := false
			skippedLarge := 0
			for _, rel := range files {
				if sb.ctx.Err() != nil {
					break
				}
				abs := filepath.Join(sb.root, filepath.FromSlash(rel))
				info, err := os.Stat(abs)
				if err != nil || info.IsDir() {
					continue
				}
				if info.Size() > 2*1024*1024 {
					skippedLarge++
					continue
				}
				data, err := sb.readFileBytes(abs)
				if err != nil || bytes.IndexByte(data, 0) != -1 {
					continue // skip binary files
				}
				lines := strings.Split(string(data), "\n")
				for idx, line := range lines {
					if re.MatchString(line) {
						if len(hits) >= maxHits {
							truncated = true
							break
						}
						trimmed := strings.TrimSpace(line)
						if len(trimmed) > 200 {
							trimmed = trimmed[:200] + "..."
						}
						hits = append(hits, hit{File: rel, Line: idx + 1, Text: trimmed})
					}
				}
				if truncated {
					break
				}
			}
			props := map[string]any{"truncated": truncated}
			if skippedLarge > 0 {
				props["skippedLargeFiles"] = skippedLarge
			}
			return toJSArrayWithProps(vm, hits, props)
		},
		"grepFn": func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 1 {
				panic(vm.NewTypeError("fs.grepFn(regex, globPattern?, optsOrMaxFns?) requires a string or RegExp as argument 1"))
			}
			rawPattern, re := parseRegexArg(vm, call.Arguments[0], "fs.grepFn(regex, globPattern?, optsOrMaxFns?)")

			globPat := "**/*"
			hasGlobArg := false
			if len(call.Arguments) >= 2 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
				if _, ok := call.Arguments[1].Export().(string); !ok {
					panic(vm.NewTypeError("fs.grepFn(regex, globPattern?, optsOrMaxFns?): second argument globPattern must be a string"))
				}
				if s := call.Arguments[1].String(); s != "" {
					globPat = s
					hasGlobArg = true
				}
			}
			maxFns := 8
			maxLines := 200
			var ignore []string
			if len(call.Arguments) >= 3 && !goja.IsUndefined(call.Arguments[2]) && !goja.IsNull(call.Arguments[2]) {
				arg2 := call.Arguments[2]
				if obj, ok := arg2.(*goja.Object); ok {
					if mv := obj.Get("maxFns"); mv != nil && !goja.IsUndefined(mv) && !goja.IsNull(mv) {
						if n := int(mv.ToInteger()); n > 0 {
							maxFns = n
						}
					}
					if ml := obj.Get("maxLines"); ml != nil && !goja.IsUndefined(ml) && !goja.IsNull(ml) {
						if n := int(ml.ToInteger()); n > 0 {
							maxLines = n
						}
					}
					ignore = extractStringSlice(vm, obj.Get("ignore"))
				} else if n := int(arg2.ToInteger()); n > 0 {
					maxFns = n
				}
			}
			if maxFns > 30 {
				maxFns = 30
			}

			files, _, err := sb.walkFiles(globPat, 2000, ignore)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			if hasGlobArg && len(files) == 0 {
				if abs0, _, rerr := sb.resolvePath(rawPattern); rerr == nil {
					if _, serr := os.Stat(abs0); serr == nil {
						panic(vm.NewTypeError(fmt.Sprintf("fs.grepFn: globPattern %q matched 0 files, while first arg %q is an existing path — arguments look swapped; signature is fs.grepFn(regex, globPattern?, optsOrMaxFns?)", globPat, rawPattern)))
					}
				}
			}

			type grepFnEntry struct {
				Found     bool   `json:"found"`
				File      string `json:"file"`
				Symbol    string `json:"symbol"`
				StartLine int    `json:"startLine"`
				EndLine   int    `json:"endLine"`
				HitLines  []int  `json:"hitLines"`
				Truncated bool   `json:"truncated,omitempty"`
				Text      string `json:"text"`
			}
			results := make([]*grepFnEntry, 0, maxFns)
			seen := make(map[string]*grepFnEntry)
			truncated := false

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
					continue
				}
				lines := strings.Split(string(data), "\n")
				var syms []symbolInfo
				symsParsed := false
				for idx, line := range lines {
					if !re.MatchString(line) {
						continue
					}
					if !symsParsed {
						syms = extractSymbolsFromLines(rel, lines)
						symsParsed = true
					}
					lineNum := idx + 1
					fn := extractFunctionAtWithSymbols(rel, lines, syms, lineNum, maxLines)
					key := fmt.Sprintf("%s:%d:%d", rel, fn.StartLine, fn.EndLine)
					if existing, ok := seen[key]; ok {
						existing.HitLines = append(existing.HitLines, lineNum)
						continue
					}
					if len(results) >= maxFns {
						truncated = true
						break
					}
					entry := &grepFnEntry{
						Found:     fn.Found,
						File:      fn.File,
						Symbol:    fn.Symbol,
						StartLine: fn.StartLine,
						EndLine:   fn.EndLine,
						HitLines:  []int{lineNum},
						Truncated: fn.Truncated,
						Text:      fn.Text,
					}
					seen[key] = entry
					results = append(results, entry)
				}
				if truncated {
					break
				}
			}
			return toJSArrayWithProps(vm, results, map[string]any{"truncated": truncated})
		},
		"edit": func(call goja.FunctionCall) goja.Value {
			if sb.readOnly {
				panic(vm.NewGoError(errors.New("fs.edit is disabled in read-only mode (code_read)")))
			}
			if len(call.Arguments) < 2 {
				panic(vm.NewTypeError("fs.edit(path, target, replacement, opts?) or fs.edit(path, [{target, replacement}], opts?)"))
			}
			abs, rel, err := sb.resolvePath(call.Arguments[0].String())
			if err != nil {
				panic(vm.NewGoError(err))
			}

			type editOpts struct {
				optional        bool
				replaceAll      bool
				wholeWord       bool
				expectCount     int
				maxReplacements int
			}

			parseOpts := func(v goja.Value, def editOpts) editOpts {
				out := def
				if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
					return out
				}
				obj := v.ToObject(vm)
				if obj == nil {
					return out
				}
				if ov := obj.Get("optional"); ov != nil && !goja.IsUndefined(ov) {
					out.optional = ov.ToBoolean()
				}
				if rv := obj.Get("replaceAll"); rv != nil && !goja.IsUndefined(rv) {
					out.replaceAll = rv.ToBoolean()
				}
				if wv := obj.Get("wholeWord"); wv != nil && !goja.IsUndefined(wv) {
					out.wholeWord = wv.ToBoolean()
				}
				if ev := obj.Get("expectCount"); ev != nil && !goja.IsUndefined(ev) && !goja.IsNull(ev) {
					if n := int(ev.ToInteger()); n > 0 {
						out.expectCount = n
					}
				}
				if mv := obj.Get("maxReplacements"); mv != nil && !goja.IsUndefined(mv) && !goja.IsNull(mv) {
					if n := int(mv.ToInteger()); n > 0 {
						out.maxReplacements = n
					}
				}
				return out
			}

			type editSpec struct {
				target      string
				replacement string
				opts        editOpts
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
					def := parseOpts(topOpts, editOpts{})
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
						specs = append(specs, editSpec{
							target:      target,
							replacement: rVal.String(),
							opts:        parseOpts(itemVal, def),
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
				specs = []editSpec{{target: target, replacement: replacement, opts: parseOpts(optsVal, editOpts{})}}
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

			errPrefix := func(idx int) string {
				if isMulti {
					return fmt.Sprintf("fs.edit: edit[%d] ", idx)
				}
				return "fs.edit: "
			}

			for idx, sp := range specs {
				spans := findLiteralMatches(curText, sp.target, sp.opts.wholeWord)
				count := len(spans)
				prev := previewTarget(sp.target)
				if sp.opts.expectCount > 0 && count != sp.opts.expectCount {
					panic(vm.NewGoError(fmt.Errorf("%starget %q expected %d occurrences in %s, found %d", errPrefix(idx), prev, sp.opts.expectCount, rel, count)))
				}
				if count == 0 {
					if sp.opts.optional {
						continue
					}
					panic(vm.NewGoError(fmt.Errorf("%starget %q not found in %s (pass { optional: true } to ignore)", errPrefix(idx), prev, rel)))
				}
				if sp.opts.maxReplacements > 0 && count > sp.opts.maxReplacements {
					panic(vm.NewGoError(fmt.Errorf("%starget %q matched %d times in %s, exceeds maxReplacements=%d", errPrefix(idx), prev, count, rel, sp.opts.maxReplacements)))
				}
				if count > 1 && !sp.opts.replaceAll && sp.opts.expectCount == 0 {
					panic(vm.NewGoError(fmt.Errorf("%starget %q matched %d times in %s (pass { replaceAll: true } or add surrounding context)", errPrefix(idx), prev, count, rel)))
				}
				if sp.opts.replaceAll || sp.opts.expectCount > 0 {
					curText = applySpanReplacements(curText, spans, sp.replacement)
					totalOcc += count
				} else {
					curText = applySpanReplacements(curText, spans[:1], sp.replacement)
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

			if err := sb.stageWrite(abs, rel, []byte(curText)); err != nil {
				panic(vm.NewGoError(err))
			}
			diff := sb.formatDiff(rel, oldText, curText)
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
			expectEnd := ""
			if len(call.Arguments) >= 5 && !goja.IsUndefined(call.Arguments[4]) && !goja.IsNull(call.Arguments[4]) {
				if obj := call.Arguments[4].ToObject(vm); obj != nil {
					if ev := obj.Get("expectStart"); ev != nil && !goja.IsUndefined(ev) && !goja.IsNull(ev) {
						expectStart = ev.String()
					}
					if ev := obj.Get("expectEnd"); ev != nil && !goja.IsUndefined(ev) && !goja.IsNull(ev) {
						expectEnd = ev.String()
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
			if expectEnd != "" && !strings.Contains(lines[endLine-1], expectEnd) {
				panic(vm.NewGoError(fmt.Errorf("fs.editLines: line %d in %s does not contain expectEnd %q (got %q)", endLine, rel, expectEnd, strings.TrimSpace(lines[endLine-1]))))
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
			if err := sb.stageWrite(abs, rel, []byte(newText)); err != nil {
				panic(vm.NewGoError(err))
			}
			diff := sb.formatDiff(rel, oldText, newText)
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
			if err := sb.stageWrite(abs, rel, []byte(content)); err != nil {
				panic(vm.NewGoError(err))
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
		"revert": func(call goja.FunctionCall) goja.Value {
			if sb.readOnly {
				panic(vm.NewGoError(errors.New("fs.revert is disabled in read-only mode (code_read)")))
			}
			count, err := sb.revertAll()
			if err != nil {
				panic(vm.NewGoError(err))
			}
			return vm.ToValue(map[string]any{"reverted": count, "dryRun": sb.dryRun})
		},
	}
	if sb.readOnly {
		delete(fsObj, "edit")
		delete(fsObj, "editLines")
		delete(fsObj, "write")
		delete(fsObj, "commit")
		delete(fsObj, "revert")
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

	// sh API (omitted entirely in read-only code_read mode)
	if !sb.readOnly {
		_ = vm.Set("sh", map[string]any{
			"run": func(call goja.FunctionCall) goja.Value {
				if !sb.allowShell {
					panic(vm.NewGoError(errors.New("sh.run is disabled (allowShell=false)")))
				}
				if len(call.Arguments) < 1 {
					panic(vm.NewTypeError("sh.run(command) requires command"))
				}
				code, stdout, stderr, timedOut, err := sb.runShellCommand(call.Arguments[0].String())
				if err != nil {
					panic(vm.NewGoError(err))
				}
				return vm.ToValue(map[string]any{
					"code":     code,
					"stdout":   stdout,
					"stderr":   stderr,
					"timedOut": timedOut,
				})
			},
			"check": func(call goja.FunctionCall) goja.Value {
				if !sb.allowShell {
					panic(vm.NewGoError(errors.New("sh.check is disabled (allowShell=false)")))
				}
				if len(call.Arguments) < 1 {
					panic(vm.NewTypeError("sh.check(command, optsOrFilterRegex?) requires command"))
				}
				filterRe := defaultCheckFailRe
				maxMatches := 40
				tailLines := 15
				if len(call.Arguments) >= 2 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
					arg1 := call.Arguments[1]
					if obj, ok := arg1.(*goja.Object); ok && obj.ClassName() != "RegExp" {
						if fv := obj.Get("filter"); fv != nil && !goja.IsUndefined(fv) && !goja.IsNull(fv) {
							_, filterRe = parseRegexArg(vm, fv, "sh.check filter")
						}
						if mv := obj.Get("maxMatches"); mv != nil && !goja.IsUndefined(mv) && !goja.IsNull(mv) {
							if n := int(mv.ToInteger()); n > 0 {
								maxMatches = n
							}
						}
						if tv := obj.Get("tailLines"); tv != nil && !goja.IsUndefined(tv) && !goja.IsNull(tv) {
							if n := int(tv.ToInteger()); n >= 0 {
								tailLines = n
							}
						}
					} else {
						_, filterRe = parseRegexArg(vm, arg1, "sh.check filter")
					}
				}
				code, stdout, stderr, timedOut, err := sb.runShellCommand(call.Arguments[0].String())
				if err != nil {
					panic(vm.NewGoError(err))
				}
				combined := stdout
				if stderr != "" {
					if combined != "" && !strings.HasSuffix(combined, "\n") {
						combined += "\n"
					}
					combined += stderr
				}
				allLines := strings.Split(strings.TrimRight(combined, "\n"), "\n")
				if len(allLines) == 1 && allLines[0] == "" {
					allLines = nil
				}
				matches := make([]string, 0, 16)
				matchCount := 0
				nonEmpty := make([]string, 0, len(allLines))
				for _, l := range allLines {
					if strings.TrimSpace(l) != "" {
						nonEmpty = append(nonEmpty, l)
					}
					if filterRe.MatchString(l) {
						matchCount++
						if len(matches) < maxMatches {
							matches = append(matches, l)
						}
					}
				}
				var tail []string
				if tailLines > 0 && len(nonEmpty) > 0 {
					startTail := len(nonEmpty) - tailLines
					if startTail < 0 {
						startTail = 0
					}
					tail = nonEmpty[startTail:]
				}
				return vm.ToValue(map[string]any{
					"ok":         code == 0 && !timedOut,
					"code":       code,
					"timedOut":   timedOut,
					"totalLines": len(allLines),
					"matchCount": matchCount,
					"matches":    matches,
					"tail":       tail,
				})
			},
		})
	}

	val, runErr := vm.RunString(req.Code)
	stdout, truncated, totalBytes, shownBytes, nextOffset, fullText := sb.out.Render()
	outputId := ""
	if truncated && fullText != "" {
		outputId = globalOutputCache.put(fullText)
	}

	if runErr != nil {
		rolledBack := sb.atomic && !sb.dryRun && len(sb.stagedOrder) > 0
		res := ExecResult{
			Stdout:     stdout,
			Truncated:  truncated,
			TotalBytes: totalBytes,
			ShownBytes: shownBytes,
			OutputId:   outputId,
			NextOffset: nextOffset,
			DryRun:     sb.dryRun,
			RolledBack: rolledBack,
			DurationMs: time.Since(start).Milliseconds(),
			Error:      runErr.Error(),
			maxOut:     maxOut,
		}
		if len(sb.committedOnDisk) > 0 {
			edited, diffs := sb.computeNetDiffs(true)
			res.EditedFiles = edited
			res.Diffs = diffs
		}
		return res
	}

	if commitErr := sb.commitStaged(); commitErr != nil {
		return ExecResult{
			Stdout:     stdout,
			Truncated:  truncated,
			TotalBytes: totalBytes,
			ShownBytes: shownBytes,
			OutputId:   outputId,
			NextOffset: nextOffset,
			DryRun:     sb.dryRun,
			DurationMs: time.Since(start).Milliseconds(),
			Error:      "atomic commit failed: " + commitErr.Error(),
			maxOut:     maxOut,
		}
	}

	edited, diffs := sb.computeNetDiffs(false)
	res := ExecResult{
		Stdout:      stdout,
		Truncated:   truncated,
		TotalBytes:  totalBytes,
		ShownBytes:  shownBytes,
		OutputId:    outputId,
		NextOffset:  nextOffset,
		DryRun:      sb.dryRun,
		EditedFiles: edited,
		Diffs:       diffs,
		DurationMs:  time.Since(start).Milliseconds(),
		maxOut:      maxOut,
	}
	if val != nil && !goja.IsUndefined(val) && !goja.IsNull(val) {
		res.Result = val.Export()
	}
	return res
}
