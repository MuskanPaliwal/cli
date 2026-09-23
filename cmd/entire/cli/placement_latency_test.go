package cli

import (
	"context"
	"net"
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

	t.Run("picks the fastest measured host", func(t *testing.T) {
		t.Parallel()
		got, ok := nearestHost(hosts, map[string]time.Duration{
			"near.entire.io": 12 * time.Millisecond,
			"far.entire.io":  230 * time.Millisecond,
		})
		require.True(t, ok)
		require.Equal(t, "near.entire.io", got)
	})

	t.Run("a small lead still wins", func(t *testing.T) {
		t.Parallel()
		// No margin: the caller asked for the nearest placement, so the nearest
		// placement is the answer even when the lead is small.
		got, ok := nearestHost(hosts, map[string]time.Duration{
			"near.entire.io": 14 * time.Millisecond,
			"far.entire.io":  20 * time.Millisecond,
		})
		require.True(t, ok)
		require.Equal(t, "near.entire.io", got)
	})

	t.Run("ignores hosts that were not measured", func(t *testing.T) {
		t.Parallel()
		got, ok := nearestHost(hosts, map[string]time.Duration{"far.entire.io": 230 * time.Millisecond})
		require.True(t, ok)
		require.Equal(t, "far.entire.io", got)
	})

	t.Run("no measurement means no nearest", func(t *testing.T) {
		t.Parallel()
		// Every probe failed, so there is nothing to choose on and the caller
		// must say so rather than fall back to first-measured-wins.
		_, ok := nearestHost(hosts, nil)
		require.False(t, ok)
	})
}

func TestFormatProbedRTT(t *testing.T) {
	t.Parallel()
	require.Equal(t, "18ms", formatProbedRTT(18*time.Millisecond, true))
	require.Equal(t, "2ms", formatProbedRTT(1600*time.Microsecond, true))
	require.Empty(t, formatProbedRTT(0, false))
}

func TestProbeAddress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		host string
		want string
	}{
		{name: "bare host takes the default port", host: "aws-us-east-2.entire.io", want: "aws-us-east-2.entire.io:443"},
		// validateClusterHost admits a bare host[:port], so a dev or self-hosted
		// cluster on another port is a legal placement. Appending 443 to it
		// produced an address that never resolves, and the placement dropped out
		// of the ranking while `git clone` reached it fine.
		{name: "an explicit port is preserved", host: "localhost:8080", want: "localhost:8080"},
		{name: "bare IPv6 is bracketed once", host: "::1", want: "[::1]:443"},
		{name: "pre-bracketed IPv6 is not double-bracketed", host: "[::1]", want: "[::1]:443"},
		{name: "bracketed IPv6 with a port is preserved", host: "[::1]:8080", want: "[::1]:8080"},
		{name: "IPv4 takes the default port", host: "127.0.0.1", want: "127.0.0.1:443"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, probeAddress(tt.host))
		})
	}
}

// TestDialLatencies covers the real dial path rather than the stub the
// selection tests inject: address construction, the concurrent fan-out, and
// what happens to a host that does not answer. It stays hermetic by probing
// loopback listeners it owns, so it needs no network and no name resolution.
func TestDialLatencies(t *testing.T) {
	t.Parallel()

	// listen returns a live loopback listener's host:port. The listener closes
	// with the test, so nothing outlives it.
	listen := func(t *testing.T) string {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })
		return ln.Addr().String()
	}

	t.Run("measures every reachable host", func(t *testing.T) {
		t.Parallel()
		// Both carry an explicit port, which is also the regression guard for
		// probeAddress: with 443 appended neither would resolve and the map
		// would come back empty.
		a, b := listen(t), listen(t)
		got := dialLatencies(t.Context(), []string{a, b})
		require.Len(t, got, 2)
		require.Contains(t, got, a)
		require.Contains(t, got, b)
		require.Positive(t, got[a])
	})

	t.Run("omits a host that refuses the connection", func(t *testing.T) {
		t.Parallel()
		// A listener closed before the probe leaves a port nothing answers on,
		// which is the reachable-then-gone case the ordering must survive.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		dead := ln.Addr().String()
		require.NoError(t, ln.Close())

		live := listen(t)
		got := dialLatencies(t.Context(), []string{live, dead})
		require.Contains(t, got, live)
		require.NotContains(t, got, dead, "an unreachable host must be absent, not present with a sentinel")
	})

	t.Run("a cancelled context measures nothing", func(t *testing.T) {
		t.Parallel()
		// The budget's deadline reaches the dials through the context. Cancelling
		// up front is the deterministic stand-in for it expiring, and proves no
		// host is credited with a measurement it never produced.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		require.Empty(t, dialLatencies(ctx, []string{listen(t)}))
	})

	t.Run("no hosts is not an error", func(t *testing.T) {
		t.Parallel()
		require.Empty(t, dialLatencies(t.Context(), nil))
	})
}
