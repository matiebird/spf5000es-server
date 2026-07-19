package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"time"
)

const recoveryUnit = "spf5000es-recovery.service"

type inverterConnection interface {
	inverterDataSource
	Connect(context.Context) error
	Close() error
}

type resetRequester interface {
	RequestReset(context.Context) error
}

type inverterAvailabilitySink interface {
	InverterAvailabilityChanged(bool)
}

type systemdResetRequester struct{}

func (systemdResetRequester) RequestReset(ctx context.Context) error {
	output, err := exec.CommandContext(ctx, "systemctl", "start", recoveryUnit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("start %s: %w: %s", recoveryUnit, err, output)
	}
	return nil
}

type RecoveringInverter struct {
	mu        sync.Mutex
	inverter  inverterConnection
	resetter  resetRequester
	config    RecoveryConfig
	sleep     func(context.Context, time.Duration) error
	now       func() time.Time
	lastReset time.Time
	sink      inverterAvailabilitySink
}

func (i *RecoveringInverter) SetAvailabilitySink(sink inverterAvailabilitySink) {
	i.mu.Lock()
	i.sink = sink
	i.mu.Unlock()
}

func NewRecoveringInverter(inverter inverterConnection, resetter resetRequester, config RecoveryConfig) *RecoveringInverter {
	return &RecoveringInverter{
		inverter: inverter,
		resetter: resetter,
		config:   config,
		sleep:    sleepContext,
		now:      time.Now,
	}
}

func (i *RecoveringInverter) Connect(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.executeWithRecoveryLocked(ctx, func() error { return i.inverter.Connect(ctx) })
}

func (i *RecoveringInverter) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.inverter.Close()
}

func (i *RecoveringInverter) ReadStatus(ctx context.Context) (map[string]any, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	var result map[string]any
	err := i.executeWithRecoveryLocked(ctx, func() error {
		var err error
		result, err = i.inverter.ReadStatus(ctx)
		return err
	})
	return result, err
}

func (i *RecoveringInverter) ReadConfig(ctx context.Context) (map[string]any, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	var result map[string]any
	err := i.executeWithRecoveryLocked(ctx, func() error {
		var err error
		result, err = i.inverter.ReadConfig(ctx)
		return err
	})
	return result, err
}

func (i *RecoveringInverter) WriteConfig(ctx context.Context, key string, value any) (any, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	var result any
	err := i.executeWithRecoveryLocked(ctx, func() error {
		var err error
		result, err = i.inverter.WriteConfig(ctx, key, value)
		return err
	})
	return result, err
}

func (i *RecoveringInverter) SyncTime(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.executeWithRecoveryLocked(ctx, func() error { return i.inverter.SyncTime(ctx) })
}

func (i *RecoveringInverter) executeWithRecoveryLocked(ctx context.Context, operation func() error) error {
	operationErr := operation()
	if operationErr == nil {
		return nil
	}
	if !isFatalModbus(operationErr) {
		return operationErr
	}
	i.notifyAvailabilityLocked(false)
	slog.Warn("inverter connection failed; starting recovery", "error", operationErr)
	if err := i.reconnectAndValidateLocked(ctx, operation); err == nil {
		i.notifyAvailabilityLocked(true)
		return nil
	} else if !isFatalModbus(err) || ctx.Err() != nil {
		return errors.Join(operationErr, err)
	} else {
		slog.Warn("inverter reconnect attempts exhausted", "error", err)
	}

	now := i.now()
	if !i.lastReset.IsZero() && now.Sub(i.lastReset) < i.config.ResetCooldown {
		return errors.Join(operationErr, fmt.Errorf("USB reset suppressed during %s cooldown", i.config.ResetCooldown))
	}
	slog.Warn("requesting privileged inverter USB reset", "unit", recoveryUnit)
	if err := i.resetter.RequestReset(ctx); err != nil {
		return errors.Join(operationErr, fmt.Errorf("request USB reset: %w", err))
	}
	i.lastReset = now
	if err := i.reconnectAndValidateLocked(ctx, operation); err != nil {
		return errors.Join(operationErr, fmt.Errorf("reconnect after USB reset: %w", err))
	}
	slog.Info("inverter recovered after USB reset")
	i.notifyAvailabilityLocked(true)
	return nil
}

func (i *RecoveringInverter) notifyAvailabilityLocked(available bool) {
	if i.sink != nil {
		i.sink.InverterAvailabilityChanged(available)
	}
}

func (i *RecoveringInverter) reconnectLocked(ctx context.Context) error {
	return i.reconnectAndValidateLocked(ctx, func() error { return nil })
}

func (i *RecoveringInverter) reconnectAndValidateLocked(ctx context.Context, validate func() error) error {
	delay := i.config.InitialBackoff
	var reconnectErr error
	for attempt := 1; attempt <= i.config.ReconnectAttempts; attempt++ {
		if err := i.inverter.Close(); err != nil {
			slog.Debug("close inverter connection during recovery", "error", err)
		}
		if err := i.sleep(ctx, delay); err != nil {
			return err
		}
		slog.Info("reconnecting inverter", "attempt", attempt, "max_attempts", i.config.ReconnectAttempts)
		if err := i.inverter.Connect(ctx); err == nil {
			if err := validate(); err == nil {
				slog.Info("inverter reconnected", "attempt", attempt)
				return nil
			} else if !isFatalModbus(err) {
				return err
			} else {
				reconnectErr = err
			}
		} else {
			reconnectErr = err
		}
		delay = min(delay*2, i.config.MaxBackoff)
	}
	return reconnectErr
}
