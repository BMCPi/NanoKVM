package timesync

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/beevik/ntp"
)

// fallbackNTPIPs are anycast NTP servers known by static IP, usable before DNS
// works (e.g. clock so far off that DNSSEC fails, or resolv.conf not written
// yet). Google and Cloudflare — same rationale as JetKVM: if these are down
// the internet is broken anyway.
var fallbackNTPIPs = []string{
	"162.159.200.1",   // time.cloudflare.com
	"162.159.200.123", // time.cloudflare.com
	"216.239.35.0",    // time.google.com
	"216.239.35.4",    // time.google.com
}

var fallbackNTPHostnames = []string{
	"time.cloudflare.com",
	"time.google.com",
	"time.apple.com",
	"time.aws.com",
	"pool.ntp.org",
}

// queryNTP races the servers in chunks of queryParallel and returns the first
// valid answer, already adjusted for the measured clock offset. Abandons the
// remaining chunks as soon as ctx is cancelled.
func queryNTP(ctx context.Context, servers []string) (time.Time, bool) {
	if len(servers) == 0 {
		return time.Time{}, false
	}
	// Shuffle so the same first server isn't hit on every sync.
	servers = slices.Clone(servers)
	rand.Shuffle(len(servers), func(i, j int) { servers[i], servers[j] = servers[j], servers[i] })

	for chunk := range slices.Chunk(servers, queryParallel) {
		if ctx.Err() != nil {
			return time.Time{}, false
		}
		if t, ok := queryNTPChunk(ctx, chunk); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

// queryNTPChunk races one chunk and collects results with drain-or-abandon
// semantics: it waits for either a valid answer or ctx to be cancelled,
// whichever comes first, rather than blocking on every straggler. The
// beevik/ntp client itself has no ctx-aware query -- QueryOptions.Timeout is
// the only per-attempt bound it offers -- so an abandoned goroutine can still
// run to that timeout; results is buffered to len(servers) so its send never
// blocks regardless of how early the collector below gives up.
func queryNTPChunk(ctx context.Context, servers []string) (time.Time, bool) {
	results := make(chan *time.Duration, len(servers))

	for _, server := range servers {
		go func() {
			resp, err := ntp.QueryWithOptions(server, ntp.QueryOptions{Timeout: queryTimeout})
			if err == nil {
				// Validate rejects kiss-of-death, stratum 0 and unsynchronized
				// servers.
				err = resp.Validate()
			}
			if err != nil {
				pkgLog().Debug("timesync: ntp query failed", slog.String("server", server), slog.Any("err", err))
				results <- nil
				return
			}
			pkgLog().Debug("timesync: ntp response",
				slog.String("server", server),
				slog.Duration("offset", resp.ClockOffset),
				slog.Int("stratum", int(resp.Stratum)),
				slog.Duration("rtt", resp.RTT))
			results <- &resp.ClockOffset
		}()
	}

	for range servers {
		select {
		case offset := <-results:
			if offset != nil {
				// Applying the offset to the current clock (rather than using
				// the server timestamp) also accounts for time spent waiting
				// here.
				return time.Now().Add(*offset), true
			}
		case <-ctx.Done():
			return time.Time{}, false
		}
	}
	return time.Time{}, false
}
