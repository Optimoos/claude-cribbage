package ai

import (
	"math"
	"time"

	"cribbage/internal/bots"
	"cribbage/internal/engine"
)

// Latency is a histogram of decision times in quarter-octave buckets, from
// about 30ns up to several minutes.
type Latency struct {
	N       int64
	Total   time.Duration
	Max     time.Duration
	buckets [latencyBuckets]int64
}

const latencyBuckets = 160

func latencyBucket(d time.Duration) int {
	if d <= 1 {
		return 0
	}
	return min(int(4*math.Log2(float64(d))), latencyBuckets-1)
}

func (l *Latency) Record(d time.Duration) {
	l.N++
	l.Total += d
	l.Max = max(l.Max, d)
	l.buckets[latencyBucket(d)]++
}

func (l *Latency) Add(o *Latency) {
	l.N += o.N
	l.Total += o.Total
	l.Max = max(l.Max, o.Max)
	for i := range l.buckets {
		l.buckets[i] += o.buckets[i]
	}
}

func (l *Latency) Mean() time.Duration {
	if l.N == 0 {
		return 0
	}
	return l.Total / time.Duration(l.N)
}

// Quantile returns the upper edge of the bucket holding quantile q, which
// overstates the true value by at most 19%.
func (l *Latency) Quantile(q float64) time.Duration {
	target := int64(math.Ceil(q * float64(l.N)))
	var seen int64
	for i, c := range l.buckets {
		seen += c
		if seen >= max(target, 1) {
			return min(time.Duration(math.Exp2(float64(i+1)/4)), l.Max)
		}
	}
	return l.Max
}

// Timed wraps a bot and records how long each decision takes, separately
// for discards and pegging plays.
type Timed struct {
	bots.Bot
	Discard, Play Latency
}

// Unwrap returns the timed bot.
func (t *Timed) Unwrap() bots.Bot { return t.Bot }

func (t *Timed) Choose(o *engine.Observation, legal []engine.Action) engine.Action {
	start := now()
	a := t.Bot.Choose(o, legal)
	d := since(start)
	if legal[0].Kind == engine.ActDiscard {
		t.Discard.Record(d)
	} else {
		t.Play.Record(d)
	}
	return a
}
