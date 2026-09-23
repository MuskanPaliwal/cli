package cli

import (
	"context"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Nearest-placement selection for the clone picker.
//
// A repo readable from several clusters is offered in alphabetical host order,
// so `aws-ap-southeast-2` outranks `us-east-1` on the letter `a` alone and a
// non-interactive caller gets an error instead of a default. `--nearest` asks
// for a measured order instead: dial each candidate, offer them nearest first,
// and resolve to the fastest without a prompt.
//
// It is opt-in. Nothing here runs unless the caller passes the flag, so the
// default clone path dials nothing and behaves exactly as it always has.
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

// placementProbePort is the port dialled to time a cluster that does not name
// one. It is the git smart-HTTP port every placement serves, so a reachable
// placement answers and an unreachable one is excluded from the ordering rather
// than offered and then failing under `git clone`.
const placementProbePort = "443"

// probeAddress builds the dial address for a cluster host.
//
// A placement host may already carry a port: validateClusterHost admits a bare
// "host[:port]" and hostFromPublicURL preserves what the cluster registry
// published, so a dev or self-hosted cluster on host:8080 is a legal placement.
// Appending 443 unconditionally turned those into "[host:8080]:443", which
// never resolves — the probe would report the placement unreachable and
// --nearest would silently rank it last or omit it while `git clone` reached it
// fine. The port that is dialled has to be the port the clone will use.
//
// IPv6 needs both halves of this: "[::1]:8080" already has a port and is
// returned as is, while a bare "::1" is bracketed exactly once. JoinHostPort
// brackets any host containing a colon, so a host that arrives pre-bracketed
// has them stripped first rather than doubled into "[[::1]]:443".
func probeAddress(host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"), placementProbePort)
}

// latencyProbe measures round-trip time to each host, keyed by host. Hosts that
// do not answer inside the budget are ABSENT from the result rather than
// present with a sentinel: "unknown" and "slow" order differently, and a
// sentinel would sort an unreachable placement into a real position.
//
// It is a field on placementPicker rather than a package var so that tests can
// substitute one without mutating shared state, which t.Parallel forbids. A nil
// probe is the DEFAULT, not an error path: without --nearest no probe is set,
// so nothing dials and the alphabetical picker stands.
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
			conn, err := dialer.DialContext(ctx, "tcp", probeAddress(host))
			if err != nil {
				return // unreachable or over budget: omitted, never guessed at
			}
			elapsed := time.Since(start)
			_ = conn.Close()
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

// nearestHost returns the fastest measured host, or false when no host was
// measured at all.
//
// There is no margin and no incumbent to beat: the caller typed --nearest, so
// the nearest placement is the answer they asked for. An earlier draft required
// a candidate to beat the default by 25ms, on the grounds that a mirror trails
// its primary and a few milliseconds do not pay for that staleness. Under an
// opt-in flag that reasoning inverts — a user who asks for the nearest and is
// handed the far one has been overruled by a rule they cannot see — and the
// "incumbent" it compared against was the alphabetically first host, which is
// not the primary and means nothing. A staleness guard belongs here only once
// the primary is identifiable (publicv1 models the role; coreapi does not yet
// carry it) and only if this ever becomes the default.
//
// False is decisive rather than a fallback to first-measured-wins: with no
// measurement there is no nearest, and the caller reports that instead of
// picking a host on a coin flip.
func nearestHost(hosts []string, rtt map[string]time.Duration) (string, bool) {
	var (
		best    string
		bestRTT time.Duration
	)
	for _, host := range hosts {
		got, ok := rtt[host]
		if !ok || (best != "" && got >= bestRTT) {
			continue
		}
		best, bestRTT = host, got
	}
	return best, best != ""
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
