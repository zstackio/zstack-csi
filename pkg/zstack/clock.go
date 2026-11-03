package zstack

import (
	"fmt"
	"time"

	"github.com/zstackio/zstack-sdk-go-v2/pkg/param"
)

// ZStack accepts an AccessKey request only when its Date header is within 15
// minutes of the management node's clock. With the default global config
// rest/checkTimeZone=false the zone in the header is ignored and only the wall
// clock time is compared, while the SDK writes the header in the process's
// local zone. ZStack tells its time zone only to administrators, so
// SyncTimeZone tries UTC offsets with a read the driver is allowed anyway
// until one is accepted.
//
// Each attempt labels the wall clock "GMT": ZStack can always parse that label,
// and it is the true zone for offset 0, which is what a management node with
// rest/checkTimeZone=true accepts.

// candidateOffsets lists UTC offsets in seconds, most likely first: China,
// then UTC, then every half hour and the zones at a quarter past the hour.
func candidateOffsets() []int {
	const halfHour = 30 * 60
	offsets := []int{8 * 3600, 0}
	seen := map[int]bool{8 * 3600: true, 0: true}
	add := func(o int) {
		if !seen[o] {
			seen[o] = true
			offsets = append(offsets, o)
		}
	}
	for o := -12 * 3600; o <= 14*3600; o += halfHour {
		add(o)
	}
	for _, o := range []int{5*3600 + 45*60, 8*3600 + 45*60, 12*3600 + 45*60} {
		add(o)
	}
	return offsets
}

// SyncTimeZone sets the process's local time zone so that the SDK signs
// requests with the management node's wall clock, and returns the UTC offset
// it found. Call it before anything else uses the clock.
func (c *Cloud) SyncTimeZone() (string, error) {
	q := param.NewQueryParam()
	q.Limit(1)
	var lastErr error
	for _, offset := range candidateOffsets() {
		time.Local = time.FixedZone("GMT", offset)
		_, err := c.cli.QueryVolume(&q)
		if err == nil {
			return "UTC" + time.Now().Format("-07:00"), nil
		}
		if !isForbidden(err) {
			return "", err
		}
		lastErr = err
	}
	return "", fmt.Errorf("ZStack rejected the AccessKey at every UTC offset; check the AccessKey and the clocks: %w", lastErr)
}
