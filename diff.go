package main

import (
	"fmt"
	"strings"
)

// diffStats counts removed and added lines in the changed region.
func diffStats(oldText, newText string) (added, removed int) {
	if oldText == newText {
		return 0, 0
	}
	oldLines := strings.Split(oldText, "\n")
	newLines := strings.Split(newText, "\n")
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	oldEnd := len(oldLines)
	newEnd := len(newLines)
	for oldEnd > prefix && newEnd > prefix && oldLines[oldEnd-1] == newLines[newEnd-1] {
		oldEnd--
		newEnd--
	}
	return newEnd - prefix, oldEnd - prefix
}

// makeDiffStat produces a one-line summary like "src/app.js (+2/-1 lines)".
func makeDiffStat(path, oldText, newText string) string {
	added, removed := diffStats(oldText, newText)
	if added == 0 && removed == 0 {
		return ""
	}
	return fmt.Sprintf("%s (+%d/-%d lines)", path, added, removed)
}

// makeSimpleDiff produces a compact unified-style diff showing changed line ranges.
func makeSimpleDiff(path, oldText, newText string, maxDiffLines int) string {
	if oldText == newText {
		return ""
	}
	if maxDiffLines <= 0 {
		maxDiffLines = 60
	}
	oldLines := strings.Split(oldText, "\n")
	newLines := strings.Split(newText, "\n")

	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
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
