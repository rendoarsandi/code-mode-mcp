package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type readFnResult struct {
	Found       bool     `json:"found"`
	File        string   `json:"file"`
	Symbol      string   `json:"symbol"`
	StartLine   int      `json:"startLine,omitempty"`
	EndLine     int      `json:"endLine,omitempty"`
	Truncated   bool     `json:"truncated,omitempty"`
	Text        string   `json:"text,omitempty"`
	Matches     []int    `json:"matches,omitempty"`
	Suggestions []string `json:"suggestions,omitempty"`
}

type symbolInfo struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Line    int    `json:"line"`
	EndLine int    `json:"endLine"`
}

type symbolSpec struct {
	re   *regexp.Regexp
	kind string
}

var symbolExtractors = []symbolSpec{
	{regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)`), "function"},
	{regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+)?=\s*(?:async\s*)?(?:<[^>]*>\s*)?(?:\(|function\b|[A-Za-z_$][\w$]*\s*=>)`), "function"},
	{regexp.MustCompile(`^\s*func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)(?:\[[^\]]*\])?\s*\(`), "func"},
	{regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)\s*\(`), "def"},
	{regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`), "class"},
	{regexp.MustCompile(`^\s*type\s+([A-Za-z_]\w*)(?:\[[^\]]*\])?\s+(?:struct|interface)\b`), "type"},
	{regexp.MustCompile(`^\s*(?:export\s+)?interface\s+([A-Za-z_$][\w$]*)`), "interface"},
	{regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:async\s+)?(?:unsafe\s+)?fn\s+([A-Za-z_]\w*)(?:<[^>]*>)?\s*\(`), "fn"},
	{regexp.MustCompile(`^\s*(?:(?:public|private|protected|static|readonly|override|abstract|async|get|set)\s+)*([A-Za-z_$][\w$]*)(?:<[^>]*>)?\s*\([^)]*\)\s*(?::\s*[^={;]+)?\{`), "method"},
	{regexp.MustCompile(`^\s*([A-Za-z_$][\w$]*)\s*:\s*(?:async\s*)?(?:function\b|\([^)]*\)\s*(?::\s*[^=]+)?=>)`), "method"},
}

// multilineMethodStart catches method declarations whose parameter list spans multiple lines, e.g.:
//   public async buildMesh(
//     cx: number,
//     cz: number,
//   ): Promise<void> {
var multilineMethodStart = regexp.MustCompile(`^\s*(?:(?:public|private|protected|static|readonly|override|abstract|async|get|set)\s+)*([A-Za-z_$][\w$]*)(?:<[^>]*>)?\s*\([^)]*$`)

var ignoredControlKeywords = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"else": true, "return": true, "try": true, "finally": true, "with": true,
	"new": true, "typeof": true, "delete": true, "throw": true, "case": true,
	"func": true, "function": true,
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			curr[j] = m
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

func suggestFromSymbols(syms []symbolInfo, target string, maxSuggestions int) []string {
	seen := make(map[string]bool)
	var all []string
	for _, sym := range syms {
		if !ignoredControlKeywords[sym.Name] && !seen[sym.Name] {
			seen[sym.Name] = true
			all = append(all, sym.Name)
		}
	}
	if len(all) == 0 {
		return nil
	}
	tl := strings.ToLower(target)
	type scored struct {
		name  string
		score int
	}
	ranked := make([]scored, 0, len(all))
	for _, name := range all {
		nl := strings.ToLower(name)
		// Lower score is better.
		s := levenshtein(tl, nl) * 10
		if strings.HasPrefix(nl, tl) || strings.HasPrefix(tl, nl) {
			s -= 60
		} else if strings.Contains(nl, tl) || strings.Contains(tl, nl) {
			s -= 40
		}
		// Shared prefix length bonus (e.g. createPlayerEntity vs createPlayerForPeer)
		shared := 0
		for shared < len(tl) && shared < len(nl) && tl[shared] == nl[shared] {
			shared++
		}
		s -= shared * 4
		ranked = append(ranked, scored{name: name, score: s})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score < ranked[j].score
		}
		return ranked[i].name < ranked[j].name
	})
	if len(ranked) > maxSuggestions {
		ranked = ranked[:maxSuggestions]
	}
	out := make([]string, len(ranked))
	for i, r := range ranked {
		out[i] = r.name
	}
	return out
}

func formatNumberedLines(lines []string, startLine int) string {
	var b strings.Builder
	for i, l := range lines {
		fmt.Fprintf(&b, "%4d: %s\n", startLine+i, l)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// braceScanState tracks multi-line lexical state (block comments, backtick strings/template literals,
// and JS regex literals) so braces inside comments/strings/regexes never corrupt block boundaries.
type braceScanState struct {
	isGo           bool
	isJS           bool
	inBlockComment bool
	inBacktick     bool
	tplExprDepths  []int // nested `${ ... }` brace depths inside JS template literals
}

func newBraceScanState(rel string) braceScanState {
	ext := strings.ToLower(filepath.Ext(rel))
	return braceScanState{
		isGo: ext == ".go",
		isJS: ext == ".js" || ext == ".mjs" || ext == ".cjs" || ext == ".ts" || ext == ".tsx" || ext == ".jsx",
	}
}

func (st *braceScanState) atCodeLevel() bool {
	return !st.inBlockComment && !st.inBacktick
}

func isRegexLeadChar(c byte) bool {
	switch c {
	case 0, '=', '(', ',', ':', ';', '[', '!', '&', '|', '?', '{', '}', '+', '-', '~', '^', '*', '%', '<', '>':
		return true
	}
	return false
}

func isRegexLeadWord(word string) bool {
	switch word {
	case "return", "case", "throw", "in", "of", "typeof", "void", "delete", "await", "yield", "else", "do":
		return true
	}
	return false
}

// scanLine advances st across line and returns brace counts (opened, net delta) and paren delta.
func (st *braceScanState) scanLine(line string) (opened int, delta int, parenDelta int) {
	inSingle, inDouble := false, false
	inRegex, inRegexClass := false, false
	escaped := false
	var lastSigByte byte
	var lastWord strings.Builder

	for i := 0; i < len(line); i++ {
		c := line[i]

		if st.inBlockComment {
			if c == '*' && i+1 < len(line) && line[i+1] == '/' {
				st.inBlockComment = false
				i++
			}
			continue
		}

		if inRegex {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if inRegexClass {
				if c == ']' {
					inRegexClass = false
				}
				continue
			}
			if c == '[' {
				inRegexClass = true
				continue
			}
			if c == '/' {
				inRegex = false
				lastSigByte = '/'
				lastWord.Reset()
			}
			continue
		}

		if inSingle {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
			} else if c == '\'' {
				inSingle = false
				lastSigByte = '\''
				lastWord.Reset()
			}
			continue
		}

		if inDouble {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inDouble = false
				lastSigByte = '"'
				lastWord.Reset()
			}
			continue
		}

		if st.inBacktick {
			if st.isGo {
				// Go raw strings have no escapes and no interpolations.
				if c == '`' {
					st.inBacktick = false
					lastSigByte = '`'
					lastWord.Reset()
				}
				continue
			}
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '`' {
				st.inBacktick = false
				lastSigByte = '`'
				lastWord.Reset()
				continue
			}
			if st.isJS && c == '$' && i+1 < len(line) && line[i+1] == '{' {
				// Enter JS template interpolation expression; braces inside are real code.
				st.inBacktick = false
				st.tplExprDepths = append(st.tplExprDepths, 1)
				lastSigByte = '{'
				lastWord.Reset()
				i++
				continue
			}
			continue
		}

		// Line comment
		if c == '/' && i+1 < len(line) && line[i+1] == '/' {
			break
		}
		// Block comment start
		if c == '/' && i+1 < len(line) && line[i+1] == '*' {
			st.inBlockComment = true
			i++
			continue
		}
		// JS regex literal heuristic
		if st.isJS && c == '/' && (isRegexLeadChar(lastSigByte) || isRegexLeadWord(lastWord.String())) {
			inRegex = true
			inRegexClass = false
			escaped = false
			continue
		}

		switch c {
		case '\'':
			inSingle = true
			escaped = false
		case '"':
			inDouble = true
			escaped = false
		case '`':
			st.inBacktick = true
			escaped = false
		case '(':
			parenDelta++
			lastSigByte = c
			lastWord.Reset()
		case ')':
			parenDelta--
			lastSigByte = c
			lastWord.Reset()
		case '{':
			if len(st.tplExprDepths) > 0 {
				st.tplExprDepths[len(st.tplExprDepths)-1]++
			} else {
				opened++
				delta++
			}
			lastSigByte = c
			lastWord.Reset()
		case '}':
			if len(st.tplExprDepths) > 0 {
				top := len(st.tplExprDepths) - 1
				st.tplExprDepths[top]--
				if st.tplExprDepths[top] == 0 {
					st.tplExprDepths = st.tplExprDepths[:top]
					st.inBacktick = true
				}
			} else {
				delta--
			}
			lastSigByte = c
			lastWord.Reset()
		default:
			if c == ' ' || c == '\t' || c == '\r' {
				// preserve lastSigByte/lastWord across spaces
			} else if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '$' {
				if lastSigByte != 'W' {
					lastWord.Reset()
				}
				lastWord.WriteByte(c)
				lastSigByte = 'W'
			} else {
				lastSigByte = c
				lastWord.Reset()
			}
		}
	}
	return opened, delta, parenDelta
}

func leadingIndent(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			n++
		} else if s[i] == '\t' {
			n += 4
		} else {
			break
		}
	}
	return n
}

// looksLikeMultilineMethod peeks ahead from a line matching `name(` to verify that the
// matching `)` is followed by `{` (optionally with a `: ReturnType` annotation) rather than `;`/`,`/`.`.
func looksLikeMultilineMethod(rel string, lines []string, startIdx int) bool {
	st := newBraceScanState(rel)
	parenDepth := 0
	maxLook := startIdx + 15
	if maxLook > len(lines) {
		maxLook = len(lines)
	}
	for i := startIdx; i < maxLook; i++ {
		opened, _, pDelta := st.scanLine(lines[i])
		parenDepth += pDelta
		if parenDepth <= 0 {
			if opened > 0 {
				return true
			}
			// Check the text after the final `)` on this line, or `{` on the immediate next line.
			trimmed := strings.TrimSpace(lines[i])
			if strings.HasSuffix(trimmed, ";") || strings.HasSuffix(trimmed, ",") {
				return false
			}
			if i+1 < maxLook && strings.HasPrefix(strings.TrimSpace(lines[i+1]), "{") {
				return true
			}
			return false
		}
	}
	return false
}

func findBlockEnd(rel string, lines []string, defIdx int, isPy bool) int {
	endIdx := defIdx
	if isPy {
		baseIndent := leadingIndent(lines[defIdx])
		for i := defIdx + 1; i < len(lines); i++ {
			trimmed := strings.TrimSpace(lines[i])
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				endIdx = i
				continue
			}
			if leadingIndent(lines[i]) <= baseIndent {
				break
			}
			endIdx = i
		}
		for endIdx > defIdx && strings.TrimSpace(lines[endIdx]) == "" {
			endIdx--
		}
		return endIdx
	}

	st := newBraceScanState(rel)
	depth := 0
	parenDepth := 0
	seenBrace := false
	for i := defIdx; i < len(lines); i++ {
		opened, delta, pDelta := st.scanLine(lines[i])
		parenDepth += pDelta
		if opened > 0 {
			seenBrace = true
		}
		depth += delta
		endIdx = i
		if seenBrace && depth <= 0 {
			break
		}
		if !seenBrace {
			// Single-expression arrow function or one-liner ending on defIdx (e.g. `const f = x => x + 1;`).
			if i == defIdx && parenDepth <= 0 {
				trimmed := strings.TrimSpace(lines[i])
				if strings.HasSuffix(trimmed, ";") {
					return defIdx
				}
			}
			// If parentheses closed and we still haven't seen `{` after 2 more lines (or 15 lines total), stop cleanly.
			if i >= defIdx+15 || (i > defIdx && parenDepth <= 0 && i >= defIdx+2) {
				trimmed := strings.TrimSpace(lines[i])
				if !strings.HasSuffix(trimmed, "{") && !strings.HasSuffix(trimmed, "=>") && !strings.HasSuffix(trimmed, ":") {
					return defIdx
				}
			}
		}
	}
	return endIdx
}

func buildFnResult(rel, symbol string, lines []string, defIdx, endIdx, maxLines int, matches []int) readFnResult {
	if maxLines <= 0 {
		maxLines = 200
	}
	if maxLines > 500 {
		maxLines = 500
	}
	// Include 2 lines of surrounding context above and below so adjacent declarations/TDZ are visible.
	startIdx := defIdx - 2
	if startIdx < 0 {
		startIdx = 0
	}
	ctxEndIdx := endIdx + 2
	if ctxEndIdx >= len(lines) {
		ctxEndIdx = len(lines) - 1
	}

	truncated := false
	if ctxEndIdx-startIdx+1 > maxLines {
		ctxEndIdx = startIdx + maxLines - 1
		truncated = true
	}

	res := readFnResult{
		Found:     true,
		File:      rel,
		Symbol:    symbol,
		StartLine: startIdx + 1,
		EndLine:   ctxEndIdx + 1,
		Truncated: truncated,
		Text:      formatNumberedLines(lines[startIdx:ctxEndIdx+1], startIdx+1),
	}
	if len(matches) > 1 {
		res.Matches = matches
	}
	return res
}

// extractSymbols returns all top-level and nested symbol declarations with 1-based line ranges,
// skipping matches inside multi-line block comments and template/raw strings.
func extractSymbols(rel, text string) []symbolInfo {
	lines := strings.Split(text, "\n")
	return extractSymbolsFromLines(rel, lines)
}

func extractSymbolsFromLines(rel string, lines []string) []symbolInfo {
	isPy := strings.EqualFold(filepath.Ext(rel), ".py")
	st := newBraceScanState(rel)
	var out []symbolInfo
	for i, line := range lines {
		atCode := st.atCodeLevel()
		if !isPy {
			st.scanLine(line)
		}
		if !atCode {
			continue
		}

		matched := false
		for _, spec := range symbolExtractors {
			if m := spec.re.FindStringSubmatch(line); len(m) == 2 {
				name := m[1]
				if !ignoredControlKeywords[name] {
					endIdx := findBlockEnd(rel, lines, i, isPy)
					out = append(out, symbolInfo{
						Name:    name,
						Kind:    spec.kind,
						Line:    i + 1,
						EndLine: endIdx + 1,
					})
				}
				matched = true
				break
			}
		}
		if !matched && !isPy {
			if m := multilineMethodStart.FindStringSubmatch(line); len(m) == 2 {
				name := m[1]
				if !ignoredControlKeywords[name] && looksLikeMultilineMethod(rel, lines, i) {
					endIdx := findBlockEnd(rel, lines, i, isPy)
					out = append(out, symbolInfo{
						Name:    name,
						Kind:    "method",
						Line:    i + 1,
						EndLine: endIdx + 1,
					})
				}
			}
		}
	}
	return out
}

// extractFunctionAt finds the innermost enclosing function/method/class containing 1-based lineNum.
// If lineNum is top-level (not inside any symbol), Found is false and Text contains ±15 lines around lineNum.
func extractFunctionAt(rel, text string, lineNum, maxLines int) readFnResult {
	lines := strings.Split(text, "\n")
	if lineNum < 1 || lineNum > len(lines) {
		return readFnResult{Found: false, File: rel}
	}
	syms := extractSymbolsFromLines(rel, lines)
	return extractFunctionAtWithSymbols(rel, lines, syms, lineNum, maxLines)
}

func extractFunctionAtWithSymbols(rel string, lines []string, syms []symbolInfo, lineNum, maxLines int) readFnResult {
	if lineNum < 1 || lineNum > len(lines) {
		return readFnResult{Found: false, File: rel}
	}
	bestIdx := -1
	bestSpan := len(lines) + 1
	for i, sym := range syms {
		if sym.Line <= lineNum && lineNum <= sym.EndLine {
			span := sym.EndLine - sym.Line
			if bestIdx < 0 || span < bestSpan || (span == bestSpan && syms[bestIdx].Kind == "class" && sym.Kind != "class") {
				bestIdx = i
				bestSpan = span
			}
		}
	}
	if bestIdx >= 0 {
		best := syms[bestIdx]
		return buildFnResult(rel, best.Name, lines, best.Line-1, best.EndLine-1, maxLines, nil)
	}

	// Fallback for top-level code: return ±15 numbered lines around lineNum with Found: false.
	startIdx := lineNum - 1 - 15
	if startIdx < 0 {
		startIdx = 0
	}
	endIdx := lineNum - 1 + 15
	if endIdx >= len(lines) {
		endIdx = len(lines) - 1
	}
	return readFnResult{
		Found:     false,
		File:      rel,
		StartLine: startIdx + 1,
		EndLine:   endIdx + 1,
		Text:      formatNumberedLines(lines[startIdx:endIdx+1], startIdx+1),
	}
}

// extractFunction locates a function/method/class definition by symbol name and returns
// its full body with 1-based line numbers so callers never miss surrounding lines.
// If multiple symbols share the name, Matches lists all 1-based definition line numbers.
func extractFunction(rel, text, symbol string, maxLines int) readFnResult {
	lines := strings.Split(text, "\n")
	syms := extractSymbolsFromLines(rel, lines)

	var matchedSyms []symbolInfo
	var matchLines []int
	for _, sym := range syms {
		if sym.Name == symbol {
			matchedSyms = append(matchedSyms, sym)
			matchLines = append(matchLines, sym.Line)
		}
	}

	if len(matchedSyms) == 0 {
		return readFnResult{
			Found:       false,
			File:        rel,
			Symbol:      symbol,
			Suggestions: suggestFromSymbols(syms, symbol, 5),
		}
	}

	// Prefer non-method declarations (function/func/def/class/type/interface/fn) if multiple match,
	// otherwise pick the first match and expose all line numbers in Matches.
	chosen := matchedSyms[0]
	for _, sym := range matchedSyms {
		if sym.Kind != "method" {
			chosen = sym
			break
		}
	}

	return buildFnResult(rel, symbol, lines, chosen.Line-1, chosen.EndLine-1, maxLines, matchLines)
}
