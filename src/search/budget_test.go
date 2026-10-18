package search

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// item costs 10 tokens for its path plus 5 per line.
func items(lines ...int) (func(int) int, func(int, int) int) {
	return func(i int) int { return lines[i] }, func(i, k int) int { return 10 + 5*k }
}

func TestFitBudgetPathsBeforeLines(t *testing.T) {
	parts, cost := items(4, 4, 4)
	// 30 for the note, 30 for three paths, 10 left for two lines.
	keep, b := fitBudget(3, parts, cost, 70)
	require.Equal(t, []int{2, 0, 0}, keep)
	require.Equal(t, Budget{MaxTokens: 70, Used: 40, Trimmed: 1, PathOnly: 2}, b)
}

func TestFitBudgetLaterItemsGetWhatIsLeft(t *testing.T) {
	parts := func(i int) int { return []int{1, 1}[i] }
	cost := func(i, k int) int { return 10 + []int{20, 5}[i]*k }
	// After both paths, 15 is left: too little for the first item's line, enough for the second's.
	keep, b := fitBudget(2, parts, cost, 65)
	require.Equal(t, []int{0, 1}, keep)
	require.Equal(t, 1, b.PathOnly)
	require.Equal(t, 0, b.Trimmed)
}

func TestFitBudgetDropsWhatDoesNotFit(t *testing.T) {
	parts, cost := items(1, 1, 1)
	keep, b := fitBudget(3, parts, cost, 54)
	require.Equal(t, []int{0, 0, -1}, keep)
	require.Equal(t, 1, b.Dropped)
	require.Equal(t, 2, b.PathOnly)
	require.Equal(t, "[token budget of 54 reached: 2 shown as path only, 1 dropped; narrow the query or raise max_tokens]", b.Note())
}

func result(id int64, lines ...string) Result {
	r := Result{FileID: id, AbsPath: fmt.Sprintf("/r/f%d.txt", id), LineCount: len(lines)}
	for i, l := range lines {
		r.Lines = append(r.Lines, Line{Number: i + 1, Segments: []Segment{{Text: l}}})
	}
	return r
}

func TestFitResults(t *testing.T) {
	results := []Result{
		result(1, strings.Repeat("a", 100), strings.Repeat("b", 100)),
		result(2, strings.Repeat("c", 100)),
	}

	out, b := FitResults(results, 0)
	require.Equal(t, results, out, "no budget")
	require.False(t, b.Cut())

	full := FormatResults(results)
	out, b = FitResults(results, EstimateTokens(full))
	require.Equal(t, results, out)
	require.False(t, b.Cut())

	out, b = FitResults(results, 85)
	require.Len(t, out, 2)
	require.Len(t, out[0].Lines, 1)
	require.Empty(t, out[1].Lines)
	require.Equal(t, 1, b.Trimmed)
	require.Equal(t, 1, b.PathOnly)
	require.LessOrEqual(t, EstimateTokens(FormatResults(out)+"\n"+b.Note()), 85)
	require.Contains(t, FormatResults(out), "/r/f1.txt (file_id 1)\n1: aaa")
	require.Contains(t, FormatResults(out), "… 1 more matching lines\n")
	require.Contains(t, FormatResults(out), "/r/f2.txt (file_id 2)\n… 1 matching lines\n")
	require.Len(t, results[0].Lines, 2, "the input is not modified")
}
