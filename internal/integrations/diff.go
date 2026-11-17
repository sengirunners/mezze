package integrations

import (
	"fmt"
	"strings"
)

// diffContext is the number of unchanged lines shown around each change.
const diffContext = 3

// maxLCSCells bounds the line-matching table; past it the changed middle is
// shown as one remove-all/add-all hunk (still exact, just not minimal).
const maxLCSCells = 4_000_000

// unifiedDiff returns a unified diff of before -> after ("" when equal).
// nil before means the file does not exist yet.
func unifiedDiff(path string, before, after []byte) string {
	if string(before) == string(after) && before != nil {
		return ""
	}
	a, b := splitLines(before), splitLines(after)
	ops := diffLines(a, b)

	var out strings.Builder
	from := "a" + path
	if before == nil {
		from = "/dev/null"
	}
	fmt.Fprintf(&out, "--- %s\n+++ b%s\n", from, path)

	// Group ops into hunks with diffContext lines of context.
	type op struct {
		kind byte // ' ', '-', '+'
		text string
		ai   int // line number in a (1-based) before this op
		bi   int
	}
	var all []op
	ai, bi := 0, 0
	for _, o := range ops {
		all = append(all, op{o.kind, o.text, ai, bi})
		switch o.kind {
		case ' ':
			ai++
			bi++
		case '-':
			ai++
		case '+':
			bi++
		}
	}
	i := 0
	for i < len(all) {
		if all[i].kind == ' ' {
			i++
			continue
		}
		start := max(i-diffContext, 0)
		end := i
		for end < len(all) {
			if all[end].kind != ' ' {
				end++
				continue
			}
			// Run of context: does another change follow within 2*context?
			j := end
			for j < len(all) && all[j].kind == ' ' {
				j++
			}
			if j < len(all) && j-end <= 2*diffContext {
				end = j
				continue
			}
			end = min(end+diffContext, len(all))
			break
		}
		var na, nb int
		for _, o := range all[start:end] {
			if o.kind != '+' {
				na++
			}
			if o.kind != '-' {
				nb++
			}
		}
		sa, sb := all[start].ai+1, all[start].bi+1
		if na == 0 {
			sa--
		}
		if nb == 0 {
			sb--
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", sa, na, sb, nb)
		for _, o := range all[start:end] {
			out.WriteByte(o.kind)
			out.WriteString(o.text)
			out.WriteByte('\n')
		}
		i = end
	}
	return out.String()
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := strings.TrimSuffix(string(b), "\n")
	return strings.Split(s, "\n")
}

type lineOp struct {
	kind byte
	text string
}

// diffLines trims the common prefix and suffix, then matches the middle by
// longest common subsequence (or replaces it wholesale when it is too big).
func diffLines(a, b []string) []lineOp {
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	var ops []lineOp
	for _, l := range a[:p] {
		ops = append(ops, lineOp{' ', l})
	}
	ma, mb := a[p:len(a)-s], b[p:len(b)-s]
	if (len(ma)+1)*(len(mb)+1) > maxLCSCells {
		for _, l := range ma {
			ops = append(ops, lineOp{'-', l})
		}
		for _, l := range mb {
			ops = append(ops, lineOp{'+', l})
		}
	} else {
		ops = append(ops, lcs(ma, mb)...)
	}
	for _, l := range a[len(a)-s:] {
		ops = append(ops, lineOp{' ', l})
	}
	return ops
}

func lcs(a, b []string) []lineOp {
	n, m := len(a), len(b)
	t := make([][]int32, n+1)
	for i := range t {
		t[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else {
				t[i][j] = max(t[i+1][j], t[i][j+1])
			}
		}
	}
	var ops []lineOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, lineOp{' ', a[i]})
			i++
			j++
		case t[i+1][j] >= t[i][j+1]:
			ops = append(ops, lineOp{'-', a[i]})
			i++
		default:
			ops = append(ops, lineOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, lineOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, lineOp{'+', b[j]})
	}
	return ops
}
