package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type synchronizedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *synchronizedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *synchronizedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func withDebugLogCapture(t *testing.T) (*synchronizedLogBuffer, func()) {
	t.Helper()
	output := &synchronizedLogBuffer{}
	slog.SetDefault(slog.New(newTextLogHandler(output, slog.LevelDebug)))
	return output, func() {
		configureLogging(slog.LevelInfo)
	}
}

var _ io.Writer = (*synchronizedLogBuffer)(nil)

func TestMQTTDiagnosticsExcludePayloadValues(t *testing.T) {
	output, restore := withDebugLogCapture(t)
	defer restore()

	client := &recordingMQTTClient{}
	statusValue := 380.5
	configValue := "Lithium"
	inverter := &fakePollInverter{
		status: map[string]any{"PV1Volt": statusValue},
		config: map[string]any{"BatteryType": configValue},
	}

	pollingService := NewPollingService(inverter, nil, PollingConfig{
		ConfigInterval: time.Minute,
		StatusInterval: 2 * time.Second,
	})
	mqttService := NewMQTTService(pollingService, MQTTConfig{
		TopicPrefix:       "growatt/spf5000es",
		HADiscoveryPrefix: "homeassistant",
		HADeviceID:        "growatt_spf5000es",
		HADeviceName:      "Growatt SPF 5000 ES",
		OperationTimeout:  time.Second,
		DisconnectTimeout: time.Second,
	})
	pollingService.SetSink(mqttService)

	if err := pollingService.Start(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	startServiceLoopsLikeMain(t, ctx, pollingService, mqttService)

	mqttService.mu.Lock()
	mqttService.connectionID++
	connectionID := mqttService.connectionID
	mqttService.mu.Unlock()
	mqttService.onConnect(client, connectionID)

	if pubs := waitForTopicPublications(t, client, "growatt/spf5000es", 2*time.Second); len(pubs) == 0 {
		t.Fatal("expected MQTT publications for diagnostics coverage")
	}
	if pubs := waitForTopicPublications(t, client, "homeassistant/device", 2*time.Second); len(pubs) == 0 {
		t.Fatal("expected discovery publication for diagnostics coverage")
	}
	cancel()
	time.Sleep(100 * time.Millisecond)

	logs := output.String()
	for _, needle := range []string{
		"polling delivered config to sink",
		"polling delivered status to sink",
		"mqtt config update queued",
		"mqtt status update queued",
		"mqtt run pending publish decision",
		"mqtt publish updates starting",
		"mqtt publishing config",
		"mqtt publishing status",
		"mqtt published",
		"mqtt discovery decision",
		"mqtt publishing discovery",
	} {
		if !strings.Contains(logs, needle) {
			t.Errorf("diagnostic log missing %q", needle)
		}
	}

	for _, secret := range []string{
		"380.5",
		"Lithium",
		"PV1Volt",
		"BatteryType",
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("diagnostic log leaked payload value %q", secret)
		}
	}
}

func TestMQTTDiagnosticsLogDeferredPublishWithoutPayload(t *testing.T) {
	output, restore := withDebugLogCapture(t)
	defer restore()

	service := NewMQTTService(noopCommandService{}, MQTTConfig{
		TopicPrefix:       "growatt/spf5000es",
		HADiscoveryPrefix: "homeassistant",
		HADeviceID:        "growatt_spf5000es",
		OperationTimeout:  time.Second,
		DisconnectTimeout: time.Second,
	})
	service.StatusUpdated(map[string]any{"PV1Volt": 99.9})

	logs := output.String()
	if !strings.Contains(logs, "mqtt status update queued") {
		t.Fatal("expected status queue diagnostic")
	}
	if strings.Contains(logs, "99.9") || strings.Contains(logs, "PV1Volt") {
		t.Fatalf("deferred publish diagnostics leaked payload: %q", logs)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go service.Run(ctx)

	service.mu.Lock()
	service.connected = true
	service.client = &recordingMQTTClient{}
	service.mu.Unlock()
	service.requestUpdatePublish()
	time.Sleep(100 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)

	logs = output.String()
	if !strings.Contains(logs, "mqtt run pending publish decision") {
		t.Fatal("expected run pending diagnostic after reconnect request")
	}
	if strings.Contains(logs, "99.9") {
		t.Fatalf("publish diagnostics leaked payload value: %q", logs)
	}
}

func TestMQTTDiagnosticsLogSkippedPublishWithoutClient(t *testing.T) {
	output, restore := withDebugLogCapture(t)
	defer restore()

	service := NewMQTTService(nil, MQTTConfig{
		TopicPrefix:      "growatt/spf5000es",
		OperationTimeout: time.Second,
	})
	service.publish("growatt/spf5000es/status/pv1_volt/state", []byte("123.4"), true)

	logs := output.String()
	if !strings.Contains(logs, "mqtt publish skipped") {
		t.Fatalf("expected skipped publish diagnostic, got: %q", logs)
	}
	if strings.Contains(logs, "123.4") {
		t.Fatalf("skipped publish diagnostic leaked payload: %q", logs)
	}
}
