package cli

import (
	"fmt"
	"os"
	"strings"
)

// printDiff shows how a deployed system file differs from its changed
// source, when the changed entry is the one live at that path.
func (a *app) printDiff(hostPath string) {
	sh := a.systemHalf()
	st, err := sh.loadState()
	if err != nil {
		return
	}
	for i := len(st.Modules) - 1; i >= 0; i-- {
		l, err := sh.layer.LoadLedger(st.Modules[i])
		if err != nil || l == nil {
			continue
		}
		for _, e := range l.Entries {
			if e.Host != hostPath {
				continue
			}
			old, err1 := os.ReadFile(hostPath)
			cur, err2 := os.ReadFile(e.Src)
			if err1 != nil || err2 != nil {
				return
			}
			for _, line := range lineDiff(string(old), string(cur)) {
				fmt.Fprintf(a.stdout, "          %s\n", line)
			}
			return
		}
	}
}

// maxDiffCells bounds the LCS table; config files are far below it.
const maxDiffCells = 4_000_000

// lineDiff returns the removed ("-") and added ("+") lines between a and b,
// in order and removals first, using a longest-common-subsequence table.
func lineDiff(a, b string) []string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	if len(x)*len(y) > maxDiffCells {
		return []string{"(too large to show a diff)"}
	}
	// lcs[i][j] is the LCS length of x[i:] and y[j:].
	lcs := make([][]int, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}

	var out []string
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			i, j = i+1, j+1
		case i < len(x) && (j == len(y) || lcs[i+1][j] >= lcs[i][j+1]):
			out = append(out, "- "+x[i])
			i++
		default:
			out = append(out, "+ "+y[j])
			j++
		}
	}
	return out
}
