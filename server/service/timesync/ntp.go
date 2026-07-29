package timesync

import (
	"math/rand/v2"
	"slices"
	"time"

	"github.com/beevik/ntp"
	log "github.com/sirupsen/logrus"
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
// valid answer, already adjusted for the measured clock offset.
func queryNTP(servers []string) (time.Time, bool) {
	if len(servers) == 0 {
		return time.Time{}, false
	}
	// Shuffle so the same first server isn't hit on every sync.
	servers = slices.Clone(servers)
	rand.Shuffle(len(servers), func(i, j int) { servers[i], servers[j] = servers[j], servers[i] })

	for chunk := range slices.Chunk(servers, queryParallel) {
		if t, ok := queryNTPChunk(chunk); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

func queryNTPChunk(servers []string) (time.Time, bool) {
	// Buffered so stragglers can finish and exit after we return.
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
				log.Debugf("timesync: ntp %s: %v", server, err)
				results <- nil
				return
			}
			log.Debugf("timesync: ntp %s: offset=%s stratum=%d rtt=%s",
				server, resp.ClockOffset, resp.Stratum, resp.RTT)
			results <- &resp.ClockOffset
		}()
	}

	for range servers {
		if offset := <-results; offset != nil {
			// Applying the offset to the current clock (rather than using the
			// server timestamp) also accounts for time spent waiting here.
			return time.Now().Add(*offset), true
		}
	}
	return time.Time{}, false
}
