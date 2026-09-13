package checkpoint

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCapPromptAttributions_KeepsFittingPayload(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`[{"checkpoint_number":1,"user_lines_added":3}]`)
	assert.Equal(t, raw, CapPromptAttributions(context.Background(), raw, "s1"))
	assert.Nil(t, CapPromptAttributions(context.Background(), nil, "s1"))
}

func TestCapPromptAttributions_DropsOversizedPayload(t *testing.T) {
	t.Parallel()
	// One byte over the cap: the field is dropped rather than truncated, since a
	// clipped JSON array is worse than an absent diagnostic.
	raw := json.RawMessage(bytes.Repeat([]byte("x"), MaxPromptAttributionsBytes+1))
	assert.Nil(t, CapPromptAttributions(context.Background(), raw, "s1"))

	exact := json.RawMessage(bytes.Repeat([]byte("x"), MaxPromptAttributionsBytes))
	assert.Equal(t, exact, CapPromptAttributions(context.Background(), exact, "s1"), "the cap is inclusive")
}
