package cassandra

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	gocql "github.com/apache/cassandra-gocql-driver/v2"
)

// durationUnits are CQL's duration units, largest first, for the part of
// a duration held in nanoseconds.
var durationUnits = []struct {
	unit string
	ns   int64
}{
	{"h", int64(time.Hour)}, {"m", int64(time.Minute)}, {"s", int64(time.Second)},
	{"ms", int64(time.Millisecond)}, {"us", int64(time.Microsecond)}, {"ns", 1},
}

// formatDuration spells a duration in CQL's literal form, 1y2mo3d4h5m6s;
// every component of a CQL duration has the same sign.
func formatDuration(d gocql.Duration) string {
	months, days, nanos := int64(d.Months), int64(d.Days), d.Nanoseconds
	var b strings.Builder
	if months < 0 || days < 0 || nanos < 0 {
		b.WriteByte('-')
		months, days, nanos = -months, -days, -nanos
	}
	if y := months / 12; y > 0 {
		fmt.Fprintf(&b, "%dy", y)
	}
	if mo := months % 12; mo > 0 {
		fmt.Fprintf(&b, "%dmo", mo)
	}
	if days > 0 {
		fmt.Fprintf(&b, "%dd", days)
	}
	for _, u := range durationUnits {
		if n := nanos / u.ns; n > 0 {
			fmt.Fprintf(&b, "%d%s", n, u.unit)
			nanos %= u.ns
		}
	}
	if s := b.String(); s != "" && s != "-" {
		return s
	}
	return "0s"
}

// parseDuration reads CQL's duration literal (1y2mo3w4d5h6m7s8ms9us10ns,
// "µs" too, an optional leading '-').
func parseDuration(s string) (gocql.Duration, error) {
	bad := fmt.Errorf("invalid duration %q: use the CQL form such as 1d12h30m", s)
	neg := strings.HasPrefix(s, "-")
	rest := strings.TrimPrefix(s, "-")
	if rest == "" {
		return gocql.Duration{}, bad
	}
	var months, days, nanos int64
	for rest != "" {
		i := 0
		for i < len(rest) && isDigit(rest[i]) {
			i++
		}
		if i == 0 || i > 18 {
			return gocql.Duration{}, bad
		}
		n, _ := strconv.ParseInt(rest[:i], 10, 64)
		rest = rest[i:]
		j := 0
		for j < len(rest) && !isDigit(rest[j]) {
			j++
		}
		unit := strings.ToLower(rest[:j])
		rest = rest[j:]
		switch unit {
		case "y":
			months += 12 * n
		case "mo":
			months += n
		case "w":
			days += 7 * n
		case "d":
			days += n
		case "h":
			nanos += n * int64(time.Hour)
		case "m":
			nanos += n * int64(time.Minute)
		case "s":
			nanos += n * int64(time.Second)
		case "ms":
			nanos += n * int64(time.Millisecond)
		case "us", "µs":
			nanos += n * int64(time.Microsecond)
		case "ns":
			nanos += n
		default:
			return gocql.Duration{}, bad
		}
	}
	if months > math.MaxInt32 || days > math.MaxInt32 {
		return gocql.Duration{}, bad
	}
	if neg {
		months, days, nanos = -months, -days, -nanos
	}
	return gocql.Duration{Months: int32(months), Days: int32(days), Nanoseconds: nanos}, nil
}
