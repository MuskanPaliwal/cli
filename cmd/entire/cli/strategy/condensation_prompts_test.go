package strategy

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/antigravity"
	"github.com/stretchr/testify/require"
)

// The prompt ladder's last rung must work from the transcript BYTES being
// checkpointed. When condensation fell back to the shadow-branch copy because
// the live path could not be read, a re-read of the path found nothing and the
// checkpoint carried a full transcript with no prompt.
func TestResolveCondensationPrompts_UsesTranscriptBytesWhenLivePathIsGone(t *testing.T) {
	t.Parallel()
	ag := antigravity.NewAntigravityAgent()
	transcript := []byte(`{"type":"USER_INPUT","content":"<USER_REQUEST>\nfirst ask\n</USER_REQUEST>"}
{"type":"PLANNER_RESPONSE"}
{"type":"USER_INPUT","content":"<USER_REQUEST>\nadd another\n</USER_REQUEST>"}
`)
	missing := filepath.Join(t.TempDir(), "gone.jsonl")

	got := resolveCondensationPrompts(context.Background(), ag, transcript, missing, 2)
	require.Equal(t, []string{"add another"}, got)

	// Without bytes in hand the rung degrades to the path-based read, which
	// finds nothing here — the behaviour this test exists to stop relying on.
	require.Nil(t, resolveCondensationPrompts(context.Background(), ag, nil, missing, 2))
}
