package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

type readFnResult struct {
	Found     bool   `json:"found"`
	File      string `json:"file"`
	Symbol    string `json:"symbol"`
	StartLine int    `json:"startLine,omitempty"`
	EndLine   int    `json:"endLine,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Text      string `json:"text,omitempty"`
}

func formatNumberedLines(lines []string, startLine int) string {
	var b strings.Builder
	for i, l := range lines {
		fmt.Fprintf(&b, "%4d: %s\n", startLine+i, l)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// braceDelta counts net '{' - '}' on a line while skipping basic quoted strings and // comments.
func braceDelta(line string) (opened int, delta int) {
	inSingle, inDouble, inBacktick := false, false, false
	escaped := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if escaped {
			escaped = false
			continue
		}
		if inSingle {
			if c == '\\' {
				escaped = true
			} else if c == '\'' {
				inSingle = false
			}
			continue
		}
		if inDouble {
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inDouble = false
			}
			continue
		}
		if inBacktick {
			if c == '\\' {
				escaped = true
			} else if c == '`' {
				inBacktick = false
			}
			continue
		}
		if c == '/' && i+1 < len(line) && line[i+1] == '/' {
			break
		}
		switch c {
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		case '`':
			inBacktick = true
		case '{':
			opened++
			delta++
		case '}':
			delta--
		}
	}
	return opened, delta
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

// extractFunction locates a function/method/class definition by symbol name and returns
// its full body with 1-based line numbers so callers never miss surrounding lines.
func extractFunction(rel, text, symbol string, maxLines int) readFnResult {
	if maxLines <= 0 {
		maxLines = 200
	}
	if maxLines > 500 {
		maxLines = 500
	}
	lines := strings.Split(text, "\n")
	q := regexp.QuoteMeta(symbol)

	// Priority definition patterns across JS/TS, Go, Python, Rust, C-like languages.
	defPatterns := []*regexp.Regexp{
		regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s+` + q + `\b`),
		regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+` + q + `\s*=\s*(?:async\s*)?(?:\(|function\b|[A-Za-z_$][\w$]*\s*=>)`),
		regexp.MustCompile(`^\s*func\s+(?:\([^)]*\)\s*)?` + q + `\b`),
		regexp.MustCompile(`^\s*(?:async\s+)?def\s+` + q + `\b`),
		regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?class\s+` + q + `\b`),
		regexp.MustCompile(`^\s*(?:pub\s+)?(?:async\s+)?fn\s+` + q + `\b`),
		regexp.MustCompile(`^\s*(?:async\s+)?` + q + `\s*\([^)]*\)\s*\{`),
		regexp.MustCompile(`^\s*` + q + `\s*:\s*(?:async\s*)?(?:function\b|\([^)]*\)\s*=>)`),
	}

	defIdx := -1
	for _, pat := range defPatterns {
		for i, line := range lines {
			if pat.MatchString(line) {
				defIdx = i
				break
			}
		}
		if defIdx >= 0 {
			break
		}
	}

	if defIdx < 0 {
		return readFnResult{Found: false, File: rel, Symbol: symbol}
	}

	isPy := strings.EqualFold(filepath.Ext(rel), ".py")
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
		// Trim trailing blank lines.
		for endIdx > defIdx && strings.TrimSpace(lines[endIdx]) == "" {
			endIdx--
		}
	} else {
		depth := 0
		seenBrace := false
		for i := defIdx; i < len(lines); i++ {
			opened, delta := braceDelta(lines[i])
			if opened > 0 {
				seenBrace = true
			}
			depth += delta
			endIdx = i
			if seenBrace && depth <= 0 {
				break
			}
			if !seenBrace && i >= defIdx+5 {
				// Single-expression arrow function or one-liner without braces.
				endIdx = defIdx
				break
			}
		}
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

	return readFnResult{
		Found:     true,
		File:      rel,
		Symbol:    symbol,
		StartLine: startIdx + 1,
		EndLine:   ctxEndIdx + 1,
		Truncated: truncated,
		Text:      formatNumberedLines(lines[startIdx:ctxEndIdx+1], startIdx+1),
	}
}
