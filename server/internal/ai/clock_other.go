//go:build !windows

package ai

import "time"

type tick = time.Time

func now() tick                  { return time.Now() }
func since(t tick) time.Duration { return time.Since(t) }
