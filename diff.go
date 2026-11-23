package main

import (
	"fmt"
	"strings"
)

// makeSimpleDiff produces a compact unified-style diff showing changed line ranges.
func makeSimpleDiff(path, oldText, newText string) string {
	if oldText == newText {
		return ""
	}
	oldLines := strings.Split(oldText, "\n")
	newLines := strings.Split(newText, "\n")

	// Find common prefix
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	// Find common suffix
	oldEnd := len(oldLines)
	newEnd := len(newLines)
	for oldEnd > prefix && newEnd > prefix && oldLines[oldEnd-1] == newLines[newEnd-1] {
		oldEnd--
		newEnd--
	}

	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", path, path)
	ctxStart := prefix - 2
	if ctxStart < 0 {
		ctxStart = 0
	}
	ctxOldEnd := oldEnd + 2
	if ctxOldEnd > len(oldLines) {
		ctxOldEnd = len(oldLines)
	}
	ctxNewEnd := newEnd + 2
	if ctxNewEnd > len(newLines) {
		ctxNewEnd = len(newLines)
	}
	fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", ctxStart+1, ctxOldEnd-ctxStart, ctxStart+1, ctxNewEnd-ctxStart)
	for i := ctxStart; i < prefix; i++ {
		b.WriteString(" " + oldLines[i] + "\n")
	}
	const maxDiffLines = 60
	shown := 0
	for i := prefix; i < oldEnd; i++ {
		if shown < maxDiffLines {
			b.WriteString("-" + oldLines[i] + "\n")
		}
		shown++
	}
	for i := prefix; i < newEnd; i++ {
		if shown < maxDiffLines {
			b.WriteString("+" + newLines[i] + "\n")
		}
		shown++
	}
	if shown > maxDiffLines {
		fmt.Fprintf(&b, "... (%d more changed lines omitted)\n", shown-maxDiffLines)
	}
	for i := oldEnd; i < ctxOldEnd; i++ {
		b.WriteString(" " + oldLines[i] + "\n")
	}
	return b.String()
}
