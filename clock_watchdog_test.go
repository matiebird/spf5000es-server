package main

import (
	"testing"
	"time"
)

func TestClockWatchdogDetectsClockAndTimezoneChanges(t *testing.T) {
	base := time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		current clockSample
		want    bool
	}{
		{
			name:    "normal passage of time",
			current: clockSample{wall: base.Add(time.Second), monotonic: time.Second, zoneName: "UTC"},
		},
		{
			name:    "wall clock jumps forward",
			current: clockSample{wall: base.Add(2 * time.Second), monotonic: time.Second, zoneName: "UTC"},
			want:    true,
		},
		{
			name:    "wall clock jumps backward",
			current: clockSample{wall: base, monotonic: time.Second, zoneName: "UTC"},
			want:    true,
		},
		{
			name:    "timezone offset changes",
			current: clockSample{wall: base.Add(time.Second), monotonic: time.Second, zoneName: "EEST", zoneOffset: 3 * 60 * 60},
			want:    true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			watchdog := &clockWatchdog{
				previous: clockSample{wall: base, zoneName: "UTC"},
				sample:   func() clockSample { return test.current },
			}
			if got := watchdog.Check(); got != test.want {
				t.Fatalf("Check() = %v, want %v", got, test.want)
			}
			if watchdog.previous != test.current {
				t.Fatal("watchdog did not retain the latest sample")
			}
		})
	}
}
