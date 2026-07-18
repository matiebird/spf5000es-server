package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type failingInverter struct {
	operation string
	err       error
}

func (i failingInverter) ReadStatus(context.Context) (map[string]any, error) {
	if i.operation == "status" {
		return nil, i.err
	}
	return map[string]any{}, nil
}

func (i failingInverter) ReadConfig(context.Context) (map[string]any, error) {
	if i.operation == "config" {
		return nil, i.err
	}
	return map[string]any{}, nil
}

func (i failingInverter) WriteConfig(context.Context, string, any) (any, error) {
	if i.operation == "write" {
		return nil, i.err
	}
	return nil, nil
}

func (i failingInverter) SyncTime(context.Context) error {
	if i.operation == "sync" {
		return i.err
	}
	return nil
}

func TestTakeLatestConfigCommandsCoalescesWithoutReordering(t *testing.T) {
	service := &PollingService{commands: make(chan configWriteCommand, pollCommandQueueSize)}
	service.commands <- configWriteCommand{key: "first", value: int64(1)}
	service.commands <- configWriteCommand{key: "second", value: int64(2)}
	service.commands <- configWriteCommand{key: "first", value: int64(3)}

	got := service.takeLatestConfigCommands()
	want := []configWriteCommand{
		{key: "first", value: int64(3)},
		{key: "second", value: int64(2)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %#v, want %#v", got, want)
	}
	if remaining := len(service.commands); remaining != 0 {
		t.Fatalf("%d commands remain queued", remaining)
	}
}

func TestPollingStopsOnInverterOperationFailure(t *testing.T) {
	failure := &fatalModbusError{Err: errors.New("serial link failed")}
	tests := []struct {
		name      string
		operation string
		prepare   func(*PollingService, time.Time)
	}{
		{
			name: "status poll", operation: "status",
			prepare: func(service *PollingService, now time.Time) {
				service.SetPollingEnabled(true)
				service.started = true
				service.statusDeadline = now
				service.watchdogDeadline = now.Add(time.Hour)
				service.timeSyncDeadline = now.Add(time.Hour)
			},
		},
		{
			name: "config poll", operation: "config",
			prepare: func(service *PollingService, now time.Time) {
				service.SetPollingEnabled(true)
				service.started = true
				service.configDeadline = now
			},
		},
		{
			name: "time sync", operation: "sync",
			prepare: func(service *PollingService, now time.Time) {
				service.started = true
				service.statusDeadline = now.Add(time.Hour)
				service.watchdogDeadline = now.Add(time.Hour)
				service.timeSyncDeadline = now
			},
		},
		{
			name: "config write", operation: "write",
			prepare: func(service *PollingService, _ time.Time) {
				service.commands <- configWriteCommand{key: "MaxChargeAmps", value: int64(30)}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)
			service := NewPollingService(
				failingInverter{operation: test.operation, err: failure}, nil,
				PollingConfig{ConfigInterval: time.Minute, StatusInterval: time.Second},
			)
			service.now = func() time.Time { return now }
			test.prepare(service, now)

			err := service.runPending(context.Background())
			if !errors.Is(err, failure) {
				t.Fatalf("runPending() error = %v, want wrapped Modbus failure", err)
			}
		})
	}
}

func BenchmarkTakeLatestConfigCommands(b *testing.B) {
	service := &PollingService{commands: make(chan configWriteCommand, pollCommandQueueSize)}
	keys := [...]string{"one", "two", "three", "four", "five", "six", "seven", "eight"}
	queueBatch := func() {
		for index := range pollCommandQueueSize {
			service.commands <- configWriteCommand{key: keys[index%len(keys)], value: int64(index)}
		}
	}
	queueBatch()
	service.takeLatestConfigCommands()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		queueBatch()
		if got := len(service.takeLatestConfigCommands()); got != len(keys) {
			b.Fatalf("got %d commands, want %d", got, len(keys))
		}
	}
}
