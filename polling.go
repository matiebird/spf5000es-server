package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync/atomic"
	"time"
)

const (
	syncTimeInterval     = 6 * time.Hour
	configReadbackDelay  = 2 * time.Second
	pollCommandQueueSize = 32
)

type inverterDataSource interface {
	ReadStatus(context.Context) (map[string]any, error)
	ReadConfig(context.Context) (map[string]any, error)
	WriteConfig(context.Context, string, any) (any, error)
	SyncTime(context.Context) error
}

type inverterDataSink interface {
	StatusUpdated(map[string]any)
	ConfigUpdated(map[string]any)
}

type inverterCommandService interface {
	QueueConfigWrite(string, any) error
	SetPollingEnabled(bool)
	RequestTimeSync() error
}

type configWriteCommand struct {
	key   string
	value any
}

type PollingService struct {
	inverter inverterDataSource
	sink     inverterDataSink
	config   PollingConfig

	commands         chan configWriteCommand
	pendingWrites    map[string]any
	latestCommands   map[string]configWriteCommand
	commandOrder     []string
	commandBatch     []configWriteCommand
	timeSyncRequests chan struct{}
	wake             chan struct{}
	watchdog         *clockWatchdog
	now              func() time.Time
	pollingEnabled   atomic.Bool
	pollingVersion   atomic.Uint64
	observedVersion  uint64

	started          bool
	configDeadline   time.Time
	statusDeadline   time.Time
	watchdogDeadline time.Time
	timeSyncDeadline time.Time
}

func NewPollingService(inverter inverterDataSource, sink inverterDataSink, config PollingConfig) *PollingService {
	return &PollingService{
		inverter:         inverter,
		sink:             sink,
		config:           config,
		commands:         make(chan configWriteCommand, pollCommandQueueSize),
		pendingWrites:    make(map[string]any),
		timeSyncRequests: make(chan struct{}, 1),
		wake:             make(chan struct{}, 1),
		watchdog:         newClockWatchdog(),
		now:              time.Now,
	}
}

func (s *PollingService) SetSink(sink inverterDataSink) {
	s.sink = sink
}

func (s *PollingService) Start() error {
	now := s.now()
	s.started = true
	s.configDeadline = now
	s.statusDeadline = now
	s.watchdogDeadline = now.Add(clockWatchdogInterval)
	s.timeSyncDeadline = now
	return nil
}

func (s *PollingService) Run(ctx context.Context) error {
	if !s.started {
		return errors.New("polling service is not started")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.runPending(ctx); err != nil {
			return err
		}
	}
}

func (s *PollingService) SetPollingEnabled(enabled bool) {
	wasEnabled := s.pollingEnabled.Swap(enabled)
	if enabled && !wasEnabled {
		s.pollingVersion.Add(1)
	}
	s.signalWake()
}

func (s *PollingService) RequestTimeSync() error {
	select {
	case s.timeSyncRequests <- struct{}{}:
	default:
	}
	s.signalWake()
	return nil
}

func (s *PollingService) QueueConfigWrite(key string, value any) error {
	select {
	case s.commands <- configWriteCommand{key: key, value: value}:
		s.signalWake()
		return nil
	default:
		return fmt.Errorf("inverter command queue is full")
	}
}

func (s *PollingService) runPending(ctx context.Context) error {
	now := s.now()
	pollingEnabled := s.pollingEnabled.Load()
	pollingVersion := s.pollingVersion.Load()
	commandsDue := len(s.commands) > 0
	configDue := pollingEnabled && s.started && !s.configDeadline.After(now)
	watchdogDue := s.started && !s.watchdogDeadline.After(now)
	timeSyncDue := s.started && !s.timeSyncDeadline.After(now)
	statusDue := pollingEnabled && s.started && !s.statusDeadline.After(now)
	if pollingEnabled && pollingVersion != s.observedVersion {
		configDue = true
		statusDue = true
		s.observedVersion = pollingVersion
	}
	select {
	case <-s.timeSyncRequests:
		timeSyncDue = true
	default:
	}
	workDue := commandsDue || configDue || watchdogDue || timeSyncDue || statusDue
	if !workDue {
		var configDeadline, statusDeadline time.Time
		if pollingEnabled {
			configDeadline = s.configDeadline
			statusDeadline = s.statusDeadline
		}
		nextDeadline := earliestDeadline(
			configDeadline,
			statusDeadline,
			s.watchdogDeadline,
			s.timeSyncDeadline,
		)
		wait := nextDeadline.Sub(now)
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.wake:
			return nil
		case <-timer.C:
			return nil
		}
	}

	if commandsDue {
		wroteConfig, err := s.drainCommands(ctx)
		if err != nil {
			return err
		}
		if wroteConfig {
			s.configDeadline = s.now().Add(configReadbackDelay)
			configDue = false
		}
	}

	if configDue {
		err := s.pollConfig(ctx)
		clear(s.pendingWrites)
		if err != nil {
			return inverterTaskError("config_poll", err)
		}
		s.configDeadline = s.now().Add(s.config.ConfigInterval)
	}

	if watchdogDue {
		s.watchdogDeadline = s.now().Add(clockWatchdogInterval)
		if s.watchdog.Check() {
			_ = s.RequestTimeSync()
			timeSyncDue = false
		}
	}

	if timeSyncDue {
		select {
		case <-s.timeSyncRequests:
		default:
		}
		err := s.inverter.SyncTime(ctx)
		s.timeSyncDeadline = s.now().Add(syncTimeInterval)
		if err != nil {
			return inverterTaskError("time_sync", err)
		}
	}

	if statusDue {
		err := s.pollStatus(ctx)
		s.statusDeadline = s.now().Add(s.config.StatusInterval)
		if err != nil {
			return inverterTaskError("status_poll", err)
		}
	}
	return nil
}

func earliestDeadline(deadlines ...time.Time) time.Time {
	var earliest time.Time
	for _, deadline := range deadlines {
		if !deadline.IsZero() && (earliest.IsZero() || deadline.Before(earliest)) {
			earliest = deadline
		}
	}
	return earliest
}

func (s *PollingService) signalWake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func inverterTaskError(name string, err error) error {
	return fmt.Errorf("inverter task %s: %w", name, err)
}

func (s *PollingService) drainCommands(ctx context.Context) (bool, error) {
	wroteConfig := false
	for _, command := range s.takeLatestConfigCommands() {
		if pending, exists := s.pendingWrites[command.key]; exists && reflect.DeepEqual(pending, command.value) {
			continue
		}
		_, err := s.inverter.WriteConfig(ctx, command.key, command.value)
		if err != nil {
			var commandErr *configCommandError
			if errors.As(err, &commandErr) {
				slog.Warn("rejected inverter config command", "key", command.key, "error", err)
				continue
			}
			return wroteConfig, fmt.Errorf("inverter task config_write: %w", err)
		}
		wroteConfig = true
		s.pendingWrites[command.key] = command.value
	}
	return wroteConfig, nil
}

func (s *PollingService) takeLatestConfigCommands() []configWriteCommand {
	count := len(s.commands)
	if count == 0 {
		return nil
	}
	if s.latestCommands == nil {
		s.latestCommands = make(map[string]configWriteCommand, count)
		s.commandOrder = make([]string, 0, count)
		s.commandBatch = make([]configWriteCommand, 0, count)
	} else {
		clear(s.latestCommands)
		clear(s.commandOrder)
		clear(s.commandBatch)
		s.commandOrder = s.commandOrder[:0]
		s.commandBatch = s.commandBatch[:0]
	}

	for range count {
		command := <-s.commands
		if _, exists := s.latestCommands[command.key]; !exists {
			s.commandOrder = append(s.commandOrder, command.key)
		}
		s.latestCommands[command.key] = command
	}
	for _, key := range s.commandOrder {
		s.commandBatch = append(s.commandBatch, s.latestCommands[key])
	}
	return s.commandBatch
}

func (s *PollingService) pollStatus(ctx context.Context) error {
	status, err := s.inverter.ReadStatus(ctx)
	if err != nil {
		return err
	}
	if s.sink != nil {
		s.sink.StatusUpdated(status)
	}
	return nil
}

func (s *PollingService) pollConfig(ctx context.Context) error {
	config, err := s.inverter.ReadConfig(ctx)
	if err != nil {
		return err
	}
	if s.sink != nil {
		s.sink.ConfigUpdated(config)
	}
	return nil
}
