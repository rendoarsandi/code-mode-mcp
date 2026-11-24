package main

import (
	"fmt"
	"strings"
)

type diffOpKind uint8

const (
	opEqual diffOpKind = iota
	opDelete
	opInsert
)

type diffOp struct {
	kind diffOpKind
	line string
}

// myersLineDiff computes a minimal line-level edit script using Myers O(ND) algorithm.
// Falls back to simple prefix/suffix diff if edit distance exceeds maxD.
func myersLineDiff(a, b []string) []diffOp {
	// Trim common prefix and suffix first for speed.
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	aEnd, bEnd := len(a), len(b)
	for aEnd > prefix && bEnd > prefix && a[aEnd-1] == b[bEnd-1] {
		aEnd--
		bEnd--
	}

	midA := a[prefix:aEnd]
	midB := b[prefix:bEnd]
	n, m := len(midA), len(midB)

	ops := make([]diffOp, 0, len(a)+len(b))
	for i := 0; i < prefix; i++ {
		ops = append(ops, diffOp{opEqual, a[i]})
	}

	if n == 0 && m == 0 {
		for i := aEnd; i < len(a); i++ {
			ops = append(ops, diffOp{opEqual, a[i]})
		}
		return ops
	}
	if n == 0 {
		for _, l := range midB {
			ops = append(ops, diffOp{opInsert, l})
		}
		for i := aEnd; i < len(a); i++ {
			ops = append(ops, diffOp{opEqual, a[i]})
		}
		return ops
	}
	if m == 0 {
		for _, l := range midA {
			ops = append(ops, diffOp{opDelete, l})
		}
		for i := aEnd; i < len(a); i++ {
			ops = append(ops, diffOp{opEqual, a[i]})
		}
		return ops
	}

	maxD := n + m
	const hardCapD = 1500
	if maxD > hardCapD {
		maxD = hardCapD
	}
	offset := maxD
	v := make([]int, 2*maxD+1)
	trace := make([][]int, 0, 64)

	foundD := -1
	for d := 0; d <= maxD; d++ {
		snap := make([]int, len(v))
		copy(snap, v)
		trace = append(trace, snap)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && midA[x] == midB[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				foundD = d
				break
			}
		}
		if foundD >= 0 {
			break
		}
	}

	if foundD < 0 {
		// Fallback when file underwent a massive rewrite (>1500 line edits).
		for _, l := range midA {
			ops = append(ops, diffOp{opDelete, l})
		}
		for _, l := range midB {
			ops = append(ops, diffOp{opInsert, l})
		}
	} else {
		// Backtrack edit script from trace.
		midOps := make([]diffOp, 0, n+m)
		x, y := n, m
		for d := foundD; d > 0; d-- {
			prevV := trace[d]
			k := x - y
			var prevK int
			if k == -d || (k != d && prevV[offset+k-1] < prevV[offset+k+1]) {
				prevK = k + 1
			} else {
				prevK = k - 1
			}
			prevX := prevV[offset+prevK]
			prevY := prevX - prevK
			for x > prevX && y > prevY {
				x--
				y--
				midOps = append(midOps, diffOp{opEqual, midA[x]})
			}
			if x == prevX {
				y--
				midOps = append(midOps, diffOp{opInsert, midB[y]})
			} else {
				x--
				midOps = append(midOps, diffOp{opDelete, midA[x]})
			}
		}
		for x > 0 && y > 0 {
			x--
			y--
			midOps = append(midOps, diffOp{opEqual, midA[x]})
		}
		// Reverse midOps into ops.
		for i := len(midOps) - 1; i >= 0; i-- {
			ops = append(ops, midOps[i])
		}
	}

	for i := aEnd; i < len(a); i++ {
		ops = append(ops, diffOp{opEqual, a[i]})
	}
	return ops
}

// diffStats returns exact added and removed line counts across all hunks.
func diffStats(oldText, newText string) (added, removed int) {
	if oldText == newText {
		return 0, 0
	}
	ops := myersLineDiff(strings.Split(oldText, "\n"), strings.Split(newText, "\n"))
	for _, op := range ops {
		switch op.kind {
		case opInsert:
			added++
		case opDelete:
			removed++
		}
	}
	return added, removed
}

// makeDiffStat produces a one-line summary like "src/app.js (+2/-1 lines)".
func makeDiffStat(path, oldText, newText string) string {
	added, removed := diffStats(oldText, newText)
	if added == 0 && removed == 0 {
		return ""
	}
	return fmt.Sprintf("%s (+%d/-%d lines)", path, added, removed)
}

// makeSimpleDiff produces a compact multi-hunk unified diff with 2 lines of context per hunk.
func makeSimpleDiff(path, oldText, newText string, maxDiffLines int) string {
	if oldText == newText {
		return ""
	}
	if maxDiffLines <= 0 {
		maxDiffLines = 60
	}
	ops := myersLineDiff(strings.Split(oldText, "\n"), strings.Split(newText, "\n"))

	// Identify non-equal ranges and group hunks separated by <= 4 equal lines.
	const ctxLines = 2
	type hunkRange struct{ start, end int }
	var hunks []hunkRange
	for i, op := range ops {
		if op.kind == opEqual {
			continue
		}
		s := i - ctxLines
		if s < 0 {
			s = 0
		}
		e := i + ctxLines + 1
		if e > len(ops) {
			e = len(ops)
		}
		if len(hunks) > 0 && s <= hunks[len(hunks)-1].end {
			if e > hunks[len(hunks)-1].end {
				hunks[len(hunks)-1].end = e
			}
		} else {
			hunks = append(hunks, hunkRange{s, e})
		}
	}
	if len(hunks) == 0 {
		return ""
	}

	// Precompute 1-based old/new line numbers at each op index.
	oldPos := make([]int, len(ops)+1)
	newPos := make([]int, len(ops)+1)
	oLine, nLine := 1, 1
	for i, op := range ops {
		oldPos[i] = oLine
		newPos[i] = nLine
		switch op.kind {
		case opEqual:
			oLine++
			nLine++
		case opDelete:
			oLine++
		case opInsert:
			nLine++
		}
	}
	oldPos[len(ops)] = oLine
	newPos[len(ops)] = nLine

	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", path, path)
	shownChanged := 0
	omittedChanged := 0

	for _, h := range hunks {
		oStart := oldPos[h.start]
		nStart := newPos[h.start]
		oCount := oldPos[h.end] - oStart
		nCount := newPos[h.end] - nStart
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", oStart, oCount, nStart, nCount)
		for i := h.start; i < h.end; i++ {
			op := ops[i]
			switch op.kind {
			case opEqual:
				if shownChanged <= maxDiffLines {
					b.WriteString(" " + op.line + "\n")
				}
			case opDelete:
				if shownChanged < maxDiffLines {
					b.WriteString("-" + op.line + "\n")
				} else {
					omittedChanged++
				}
				shownChanged++
			case opInsert:
				if shownChanged < maxDiffLines {
					b.WriteString("+" + op.line + "\n")
				} else {
					omittedChanged++
				}
				shownChanged++
			}
		}
	}
	if omittedChanged > 0 {
		fmt.Fprintf(&b, "... (%d more changed lines omitted)\n", omittedChanged)
	}
	return b.String()
}
