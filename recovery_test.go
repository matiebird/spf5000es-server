package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

type recoveryTestInverter struct {
	connectErrors []error
	statusErrors  []error
	connects      int
	closes        int
	statusReads   int
}

func (i *recoveryTestInverter) Connect(context.Context) error {
	i.connects++
	if len(i.connectErrors) == 0 {
		return nil
	}
	err := i.connectErrors[0]
	i.connectErrors = i.connectErrors[1:]
	return err
}

func (i *recoveryTestInverter) Close() error {
	i.closes++
	return nil
}

func (i *recoveryTestInverter) ReadStatus(context.Context) (map[string]any, error) {
	i.statusReads++
	if len(i.statusErrors) > 0 {
		err := i.statusErrors[0]
		i.statusErrors = i.statusErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	return map[string]any{"ok": true}, nil
}

func (*recoveryTestInverter) ReadConfig(context.Context) (map[string]any, error) {
	return map[string]any{}, nil
}
func (*recoveryTestInverter) WriteConfig(context.Context, string, any) (any, error) {
	return nil, nil
}
func (*recoveryTestInverter) SyncTime(context.Context) error { return nil }

type recoveryTestResetter struct {
	requests int
	err      error
}

type recoveryTestAvailabilitySink struct {
	states []bool
}

func (s *recoveryTestAvailabilitySink) InverterAvailabilityChanged(available bool) {
	s.states = append(s.states, available)
}

func (r *recoveryTestResetter) RequestReset(context.Context) error {
	r.requests++
	return r.err
}

func testRecoveringInverter(inverter *recoveryTestInverter, resetter *recoveryTestResetter) *RecoveringInverter {
	result := NewRecoveringInverter(inverter, resetter, RecoveryConfig{
		ReconnectAttempts: 2,
		InitialBackoff:    time.Millisecond,
		MaxBackoff:        2 * time.Millisecond,
		ResetCooldown:     time.Minute,
	})
	result.sleep = func(context.Context, time.Duration) error { return nil }
	result.now = func() time.Time { return time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC) }
	return result
}

func TestRecoveringInverterReconnectsAndRetriesOperation(t *testing.T) {
	failure := &fatalModbusError{Err: errors.New("serial link failed")}
	inverter := &recoveryTestInverter{
		statusErrors:  []error{failure, nil},
		connectErrors: []error{failure, nil},
	}
	resetter := &recoveryTestResetter{}
	recovering := testRecoveringInverter(inverter, resetter)
	sink := &recoveryTestAvailabilitySink{}
	recovering.SetAvailabilitySink(sink)

	status, err := recovering.ReadStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status["ok"] != true || inverter.statusReads != 2 {
		t.Fatalf("status = %#v after %d reads, want successful retry", status, inverter.statusReads)
	}
	if inverter.connects != 2 || resetter.requests != 0 {
		t.Fatalf("connects = %d, resets = %d; want 2 connects and no reset", inverter.connects, resetter.requests)
	}
	if len(sink.states) != 2 || sink.states[0] || !sink.states[1] {
		t.Fatalf("availability states = %v, want [false true]", sink.states)
	}
}

func TestRecoveringInverterResetsAfterReconnectsAreExhausted(t *testing.T) {
	failure := &fatalModbusError{Err: errors.New("device unavailable")}
	inverter := &recoveryTestInverter{
		statusErrors:  []error{failure, nil},
		connectErrors: []error{failure, failure, nil},
	}
	resetter := &recoveryTestResetter{}
	recovering := testRecoveringInverter(inverter, resetter)

	if _, err := recovering.ReadStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resetter.requests != 1 || inverter.connects != 3 {
		t.Fatalf("resets = %d, connects = %d; want one reset and three connects", resetter.requests, inverter.connects)
	}
}

func TestRecoveringInverterRequiresSuccessfulOperationBeforeDeclaringRecovery(t *testing.T) {
	failure := &fatalModbusError{Err: errors.New("inverter does not respond")}
	inverter := &recoveryTestInverter{
		statusErrors: []error{failure, failure, failure, nil},
	}
	resetter := &recoveryTestResetter{}
	recovering := testRecoveringInverter(inverter, resetter)

	if _, err := recovering.ReadStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resetter.requests != 1 {
		t.Fatalf("reset requests = %d, want one after reopened links still failed validation", resetter.requests)
	}
	if inverter.connects != 3 || inverter.statusReads != 4 {
		t.Fatalf("connects = %d, status reads = %d; want 3 connects and 4 validation reads", inverter.connects, inverter.statusReads)
	}
}

func TestRecoveringInverterDoesNotRecoverNonModbusFailure(t *testing.T) {
	failure := errors.New("decode failed")
	inverter := &recoveryTestInverter{statusErrors: []error{failure}}
	resetter := &recoveryTestResetter{}
	recovering := testRecoveringInverter(inverter, resetter)

	_, err := recovering.ReadStatus(context.Background())
	if !errors.Is(err, failure) {
		t.Fatalf("error = %v, want decode failure", err)
	}
	if inverter.connects != 0 || resetter.requests != 0 {
		t.Fatalf("connects = %d, resets = %d; want no recovery", inverter.connects, resetter.requests)
	}
}

func TestRecoveringInverterSuppressesRepeatedResetDuringCooldown(t *testing.T) {
	failure := &fatalModbusError{Err: errors.New("device unavailable")}
	inverter := &recoveryTestInverter{
		statusErrors:  []error{failure},
		connectErrors: []error{failure, failure},
	}
	resetter := &recoveryTestResetter{}
	recovering := testRecoveringInverter(inverter, resetter)
	recovering.lastReset = recovering.now().Add(-time.Second)

	_, err := recovering.ReadStatus(context.Background())
	if err == nil {
		t.Fatal("cooldown failure was not returned")
	}
	if resetter.requests != 0 {
		t.Fatalf("reset requests = %d, want zero during cooldown", resetter.requests)
	}
}
