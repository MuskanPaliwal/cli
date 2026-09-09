package gitrepo

import (
	"slices"
	"strings"
	"testing"
)

func TestChunkLiteralPathspecsBoundsCommandsWithoutDroppingPaths(t *testing.T) {
	t.Parallel()

	paths := []string{
		strings.Repeat("a", 40),
		strings.Repeat("b", 40),
		strings.Repeat("c", 120), // A single path larger than the budget stays alone.
		strings.Repeat("d", 40),
	}
	const budget = 100
	chunks := chunkLiteralPathspecs(paths, budget)

	var got []string
	for _, chunk := range chunks {
		var size int
		for _, pathspec := range chunk {
			got = append(got, strings.TrimPrefix(pathspec, ":(literal)"))
			size += len(pathspec) + 1
		}
		if len(chunk) > 1 && size > budget {
			t.Fatalf("multi-path chunk has size %d, budget %d", size, budget)
		}
	}
	if !slices.Equal(got, paths) {
		t.Fatalf("chunked paths = %q, want %q", got, paths)
	}
}
