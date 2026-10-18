package search

import (
	"fmt"
	"sort"
	"strings"
)

// noteReserve is kept free in a budget for the note saying it was reached.
const noteReserve = 30

// EstimateTokens approximates how many model tokens s costs, at about four
// bytes per token. Real tokenizers differ by model, so budgets are approximate.
func EstimateTokens(s string) int {
	return (len(s) + 3) / 4
}

// Budget reports what a token budget cut from a response.
type Budget struct {
	MaxTokens int `json:"max_tokens"`
	// Used is the estimated tokens of what was kept.
	Used int `json:"used"`
	// Trimmed items lost some of their lines.
	Trimmed int `json:"trimmed,omitempty"`
	// PathOnly items were cut down to their path.
	PathOnly int `json:"path_only,omitempty"`
	// Dropped items did not fit at all.
	Dropped int `json:"dropped,omitempty"`
}

// Cut reports whether the budget removed anything.
func (b Budget) Cut() bool {
	return b.Trimmed+b.PathOnly+b.Dropped > 0
}

// Note describes what was cut, or is empty if nothing was.
func (b Budget) Note() string {
	if !b.Cut() {
		return ""
	}
	var parts []string
	if b.Trimmed > 0 {
		parts = append(parts, fmt.Sprintf("%d trimmed", b.Trimmed))
	}
	if b.PathOnly > 0 {
		parts = append(parts, fmt.Sprintf("%d shown as path only", b.PathOnly))
	}
	if b.Dropped > 0 {
		parts = append(parts, fmt.Sprintf("%d dropped", b.Dropped))
	}
	return fmt.Sprintf("[token budget of %d reached: %s; narrow the query or raise max_tokens]", b.MaxTokens, strings.Join(parts, ", "))
}

// fitBudget decides how much of each of n items fits in maxTokens. parts(i)
// is how many lines item i has, and cost(i, k) the tokens it costs with only
// its first k lines (k == 0 is its path alone, which must cost the least).
// Every item that fits gets its path first, in order; the rest of the budget
// then goes to lines, best ranked items first. It returns how many lines of
// each item to keep, or -1 to drop the item.
func fitBudget(n int, parts func(i int) int, cost func(i, k int) int, maxTokens int) ([]int, Budget) {

	keep := make([]int, n)
	b := Budget{MaxTokens: maxTokens}
	left := maxTokens - noteReserve

	// Paths, until one does not fit.
	shown := n
	for i := range n {
		c := cost(i, 0)
		if c > left {
			shown = i
			break
		}
		left -= c
		b.Used += c
	}
	for i := shown; i < n; i++ {
		keep[i] = -1
	}
	b.Dropped = n - shown

	// Lines, as many as fit, until the budget runs out.
	for i := range shown {
		base, all := cost(i, 0), parts(i)
		// Cost only grows with k, so find the most lines that still fit.
		k := sort.Search(all+1, func(k int) bool { return cost(i, k)-base > left }) - 1
		keep[i] = k
		extra := cost(i, k) - base
		left -= extra
		b.Used += extra

		switch {
		case k == all:
		case k == 0:
			b.PathOnly++
		default:
			b.Trimmed++
		}
	}

	return keep, b
}

// FitResults cuts search results down to about maxTokens of FormatResults
// output: results keep as many matching lines as fit, then only their path.
// maxTokens <= 0 means no limit.
func FitResults(results []Result, maxTokens int) ([]Result, Budget) {

	if maxTokens <= 0 {
		return results, Budget{}
	}
	if used := EstimateTokens(FormatResults(results)); used <= maxTokens {
		return results, Budget{MaxTokens: maxTokens, Used: used}
	}

	withLines := func(r Result, k int) Result {
		r.Lines = r.Lines[:k]
		if k == 0 {
			r.Snippet = nil
		}
		return r
	}

	keep, b := fitBudget(len(results),
		func(i int) int { return len(results[i].Lines) },
		func(i, k int) int { return EstimateTokens(formatResult(withLines(results[i], k))) + 1 },
		maxTokens)

	out := make([]Result, 0, len(results))
	for i, k := range keep {
		if k >= 0 {
			out = append(out, withLines(results[i], k))
		}
	}
	return out, b
}

// FormatResults renders results as plain text, the way they are given to models.
func FormatResults(results []Result) string {
	var sb strings.Builder
	for i, r := range results {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(formatResult(r))
	}
	return sb.String()
}

func formatResult(r Result) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (file_id %d)\n", r.AbsPath, r.FileID)
	for _, l := range r.Lines {
		fmt.Fprintf(&sb, "%d: %s\n", l.Number, PlainText(l.Segments))
	}
	if len(r.Lines) == 0 && len(r.Snippet) > 0 {
		fmt.Fprintf(&sb, "%s\n", PlainText(r.Snippet))
	}
	switch more := r.LineCount - len(r.Lines); {
	case more > 0 && len(r.Lines) == 0:
		fmt.Fprintf(&sb, "… %d matching lines\n", more)
	case more > 0:
		fmt.Fprintf(&sb, "… %d more matching lines\n", more)
	}
	return sb.String()
}

// PlainText joins segments without marking matches.
func PlainText(segs []Segment) string {
	var sb strings.Builder
	for _, s := range segs {
		sb.WriteString(s.Text)
	}
	return sb.String()
}
