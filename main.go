package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
)

var buildRevision = ""

type serviceRunner interface {
	Run(context.Context) error
}

func main() {
	if err := run(); err != nil {
		slog.Error("service stopped with error", "error", err)
		os.Exit(1)
	}
}

func run() (runErr error) {
	config, err := readConfig("config.ini")
	if err != nil {
		return err
	}
	configureLogging(config.LogLevel)
	slog.Info("starting service", "revision", revision())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rawInverter := NewInverter(config.Modbus)
	inverter := NewRecoveringInverter(rawInverter, systemdResetRequester{}, config.Recovery)
	pollingService := NewPollingService(inverter, nil, config.Polling)
	mqttService := NewMQTTService(pollingService, config.MQTT)
	pollingService.SetSink(mqttService)
	inverter.SetAvailabilitySink(mqttService)
	defer func() {
		mqttService.Stop()
		if err := inverter.Close(); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("close inverter: %w", err))
		}
	}()

	logConfig(slog.Default(), config)
	if err := inverter.Connect(ctx); err != nil {
		return err
	}
	if err := pollingService.Start(); err != nil {
		return err
	}
	if err := mqttService.Start(); err != nil {
		return err
	}

	slog.Info("service loop started")
	if err := runServiceLoops(ctx, stop, pollingService, mqttService); err != nil {
		return err
	}
	slog.Info("shutdown requested")
	return nil
}

func revision() string {
	if buildRevision != "" {
		return buildRevision
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var vcsRevision string
	modified := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			vcsRevision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if vcsRevision == "" {
		return "unknown"
	}
	if modified {
		return vcsRevision + "-dirty"
	}
	return vcsRevision
}

func runServiceLoops(ctx context.Context, stop context.CancelFunc, services ...serviceRunner) error {
	errCh := make(chan error, len(services))
	for _, service := range services {
		go func() { errCh <- service.Run(ctx) }()
	}

	var runErr error
	for range services {
		err := <-errCh
		stop()
		if err != nil && !errors.Is(err, context.Canceled) {
			runErr = errors.Join(runErr, err)
		}
	}
	return runErr
}
