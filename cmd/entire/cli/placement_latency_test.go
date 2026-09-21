package cli

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOrderHostsByLatency(t *testing.T) {
	t.Parallel()

	t.Run("orders measured hosts nearest first", func(t *testing.T) {
		t.Parallel()
		// The case the feature exists for: alphabetical puts the far cluster
		// first purely because of the letter `a`.
		hosts := []string{"aws-ap-southeast-2.entire.io", "aws-us-east-2.entire.io"}
		got := orderHostsByLatency(hosts, map[string]time.Duration{
			"aws-ap-southeast-2.entire.io": 240 * time.Millisecond,
			"aws-us-east-2.entire.io":      18 * time.Millisecond,
		})
		require.Equal(t, []string{"aws-us-east-2.entire.io", "aws-ap-southeast-2.entire.io"}, got)
	})

	t.Run("unmeasured hosts sort after measured ones in input order", func(t *testing.T) {
		t.Parallel()
		hosts := []string{"a.entire.io", "b.entire.io", "c.entire.io"}
		got := orderHostsByLatency(hosts, map[string]time.Duration{"c.entire.io": 5 * time.Millisecond})
		require.Equal(t, []string{"c.entire.io", "a.entire.io", "b.entire.io"}, got)
	})

	t.Run("no measurements preserves the input order", func(t *testing.T) {
		t.Parallel()
		// Every probe failing must leave the picker exactly as it was before
		// this feature existed, not reshuffled by a partial signal.
		hosts := []string{"a.entire.io", "b.entire.io"}
		require.Equal(t, hosts, orderHostsByLatency(hosts, nil))
	})
}

func TestNearestHost(t *testing.T) {
	t.Parallel()

	hosts := []string{"near.entire.io", "far.entire.io"}

	t.Run("picks a host that clearly beats the incumbent", func(t *testing.T) {
		t.Parallel()
		got, ok := nearestHost(hosts, "far.entire.io", map[string]time.Duration{
			"near.entire.io": 12 * time.Millisecond,
			"far.entire.io":  230 * time.Millisecond,
		})
		require.True(t, ok)
		require.Equal(t, "near.entire.io", got)
	})

	t.Run("keeps the incumbent inside the margin", func(t *testing.T) {
		t.Parallel()
		// 9ms of difference is not worth the staleness a mirror carries.
		_, ok := nearestHost(hosts, "far.entire.io", map[string]time.Duration{
			"near.entire.io": 21 * time.Millisecond,
			"far.entire.io":  30 * time.Millisecond,
		})
		require.False(t, ok)
	})

	t.Run("keeps the incumbent when the incumbent was not measured", func(t *testing.T) {
		t.Parallel()
		// One arm of the comparison is missing, so there is no comparison —
		// only a preference for whichever host answered a probe.
		_, ok := nearestHost(hosts, "far.entire.io", map[string]time.Duration{
			"near.entire.io": 12 * time.Millisecond,
		})
		require.False(t, ok)
	})

	t.Run("keeps the incumbent when it is already the nearest", func(t *testing.T) {
		t.Parallel()
		_, ok := nearestHost(hosts, "near.entire.io", map[string]time.Duration{
			"near.entire.io": 12 * time.Millisecond,
			"far.entire.io":  230 * time.Millisecond,
		})
		require.False(t, ok)
	})
}

func TestFormatProbedRTT(t *testing.T) {
	t.Parallel()
	require.Equal(t, "18ms", formatProbedRTT(18*time.Millisecond, true))
	require.Equal(t, "2ms", formatProbedRTT(1600*time.Microsecond, true))
	require.Empty(t, formatProbedRTT(0, false))
}
