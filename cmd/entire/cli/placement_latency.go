package cli

import (
	"context"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Nearest-placement selection for the clone picker.
//
// A repo readable from several clusters used to be offered in alphabetical
// host order, so `aws-ap-southeast-2` outranked `us-east-1` on the letter `a`
// alone and a non-interactive caller got an error instead of a default. The
// ordering here replaces that with a measured one: dial each candidate, order
// nearest first, and let a clearly-closer placement become the default.
//
// The measurement is entirely client-side. Nothing is sent to the server about
// where the caller is, and no placement is inferred from an IP address — the
// CLI times its own connections and keeps the answer in memory for the length
// of one command.

// placementProbeBudget caps the whole probe, not each dial: every candidate is
// dialled concurrently under one deadline, so the wall-clock cost of ordering
// the picker is this value once, whatever the placement count.
//
// It is deliberately short. The probe is an optimisation with a working
// fallback, and `repo remote url` exists to have its stdout captured by
// `$(…)`, where a second of silence reads as a hung shell.
const placementProbeBudget = 400 * time.Millisecond

// placementProbePort is the port dialled to time a cluster. It is the git
// smart-HTTP port every placement already serves, so a reachable placement
// answers and an unreachable one is excluded from the ordering rather than
// offered and then failing under `git clone`.
const placementProbePort = "443"

// placementLatencyMargin is how much closer a placement must be than the
// incumbent before it is chosen WITHOUT a human in the loop. Below it the two
// are treated as equally near and the incumbent wins.
//
// The margin exists because near and correct are different questions. A mirror
// trails its primary by a replication delay, so trading the incumbent for a
// few milliseconds buys latency nobody notices and adds staleness somebody
// eventually debugs. A margin this size only ever fires on a genuine
// inter-region difference, which is the case this feature is for.
const placementLatencyMargin = 25 * time.Millisecond

// latencyProbe measures round-trip time to each host, keyed by host. Hosts that
// do not answer inside the budget are ABSENT from the result rather than
// present with a sentinel: "unknown" and "slow" order differently, and a
// sentinel would sort an unreachable placement into a real position.
//
// It is a field on placementPicker rather than a package var so that tests can
// substitute one without mutating shared state, which t.Parallel forbids. A nil
// probe disables latency ordering and restores the alphabetical picker.
type latencyProbe func(ctx context.Context, hosts []string) map[string]time.Duration

// dialLatencies times a TCP connect to each host concurrently.
//
// TCP connect, not a TLS handshake or an HTTP request: it is one round trip
// against a port that is already open, it authenticates nothing and sends no
// bytes that identify the caller or the repo, and its ratio between placements
// is what the ordering needs. An absolute number would need the handshake; a
// comparison does not.
func dialLatencies(ctx context.Context, hosts []string) map[string]time.Duration {
	ctx, cancel := context.WithTimeout(ctx, placementProbeBudget)
	defer cancel()

	var (
		mu  sync.Mutex
		out = make(map[string]time.Duration, len(hosts))
		wg  sync.WaitGroup
	)
	var dialer net.Dialer
	for _, host := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, placementProbePort))
			if err != nil {
				return // unreachable or over budget: omitted, never guessed at
			}
			elapsed := time.Since(start)
			_ = conn.Close() //nolint:errcheck // probe socket, nothing was written
			mu.Lock()
			defer mu.Unlock()
			out[host] = elapsed
		}()
	}
	wg.Wait()
	return out
}

// orderHostsByLatency returns hosts nearest-first: measured hosts ascending by
// round trip, then every unmeasured host in the order given.
//
// Unmeasured hosts stay in the caller's order and stay OFFERED. A placement
// that did not answer a probe inside 400ms is not thereby a bad placement — a
// dropped SYN, a paused laptop or a corporate proxy produce the same silence —
// so it loses its position in the ordering, not its place in the list.
func orderHostsByLatency(hosts []string, rtt map[string]time.Duration) []string {
	ordered := make([]string, len(hosts))
	copy(ordered, hosts)
	sort.SliceStable(ordered, func(i, j int) bool {
		ri, iok := rtt[ordered[i]]
		rj, jok := rtt[ordered[j]]
		if iok != jok {
			return iok
		}
		if !iok {
			return false // both unmeasured: SliceStable keeps the caller's order
		}
		return ri < rj
	})
	return ordered
}

// nearestHost reports the host to default to, given the incumbent the caller
// would otherwise have used.
//
// It answers false — meaning "keep the incumbent, and say so" — in every case
// short of a clear win: no measurement for the candidate, no measurement for
// the incumbent to compare against, or a lead inside placementLatencyMargin.
// An unmeasured incumbent is deliberately decisive: without both numbers there
// is no comparison, only a preference for whichever host happened to answer.
func nearestHost(hosts []string, incumbent string, rtt map[string]time.Duration) (string, bool) {
	incumbentRTT, ok := rtt[incumbent]
	if !ok {
		return "", false
	}
	best, bestRTT := incumbent, incumbentRTT
	for _, host := range hosts {
		got, ok := rtt[host]
		if !ok || got >= bestRTT {
			continue
		}
		best, bestRTT = host, got
	}
	if best == incumbent || incumbentRTT-bestRTT < placementLatencyMargin {
		return "", false
	}
	return best, true
}

// formatProbedRTT renders a measured round trip for the picker label, or ""
// when the host was not measured. Whole milliseconds: the picker is choosing
// between regions, where the differences are tens of milliseconds, and a
// decimal invites reading a single sample as a benchmark.
func formatProbedRTT(rtt time.Duration, ok bool) string {
	if !ok {
		return ""
	}
	return strconv.Itoa(int(rtt.Round(time.Millisecond)/time.Millisecond)) + "ms"
}
