package main

import (
	"log/slog"
	"time"
)

const (
	clockWatchdogInterval = time.Second
	clockJumpTolerance    = 500 * time.Millisecond
)

type clockSample struct {
	wall       time.Time
	monotonic  time.Duration
	zoneName   string
	zoneOffset int
}

type clockWatchdog struct {
	sample   func() clockSample
	previous clockSample
}

func newClockWatchdog() *clockWatchdog {
	start := time.Now()
	watchdog := &clockWatchdog{}
	watchdog.sample = func() clockSample {
		now := time.Now()
		zoneName, zoneOffset := now.Zone()
		return clockSample{
			wall:       now.Round(0),
			monotonic:  now.Sub(start),
			zoneName:   zoneName,
			zoneOffset: zoneOffset,
		}
	}
	watchdog.previous = watchdog.sample()

	return watchdog
}

func (w *clockWatchdog) Check() bool {
	current := w.sample()
	previous := w.previous
	w.previous = current

	wallElapsed := current.wall.Sub(previous.wall)
	monotonicElapsed := current.monotonic - previous.monotonic
	jump := wallElapsed - monotonicElapsed
	clockJumped := absDuration(jump) >= clockJumpTolerance
	zoneChanged := current.zoneName != previous.zoneName ||
		current.zoneOffset != previous.zoneOffset
	if !clockJumped && !zoneChanged {
		return false
	}

	slog.Info("host clock change detected",
		"jump", jump,
		"timezone_change", zoneChanged,
	)
	return true
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}
