package ratelimit

import (
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	l := New(3, time.Minute)
	now := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		if !l.Take("a", now) {
			t.Fatalf("take %d refused within the burst", i)
		}
	}
	if l.Take("a", now) || l.Allow("a", now) {
		t.Fatal("allowed past the burst")
	}
	if !l.Take("b", now) {
		t.Fatal("keys share a bucket")
	}
	if l.Allow("a", now.Add(59*time.Second)) {
		t.Fatal("refilled early")
	}
	if !l.Take("a", now.Add(time.Minute)) || l.Take("a", now.Add(time.Minute)) {
		t.Fatal("did not refill exactly one token a minute")
	}
	l.sweep(now.Add(time.Hour))
	if len(l.buckets) != 0 {
		t.Fatalf("sweep kept %d refilled buckets", len(l.buckets))
	}
}
