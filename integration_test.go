package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakePollInverter struct {
	mu sync.Mutex

	status map[string]any
	config map[string]any
}

func (i *fakePollInverter) ReadStatus(context.Context) (map[string]any, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.status, nil
}

func (i *fakePollInverter) ReadConfig(context.Context) (map[string]any, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.config, nil
}

func (i *fakePollInverter) WriteConfig(context.Context, string, any) (any, error) {
	return nil, nil
}

func (i *fakePollInverter) SyncTime(context.Context) error {
	return nil
}

func startServiceLoopsLikeMain(t *testing.T, ctx context.Context, polling *PollingService, mqtt *MQTTService) {
	t.Helper()
	errCh := make(chan error, 2)
	go func() { errCh <- polling.Run(ctx) }()
	go func() { errCh <- mqtt.Run(ctx) }()
	go func() {
		select {
		case err := <-errCh:
			if err != nil && err != context.Canceled {
				t.Errorf("service loop stopped: %v", err)
			}
		case <-ctx.Done():
		}
	}()
}

func waitForTopicPublications(t *testing.T, client *recordingMQTTClient, prefix string, timeout time.Duration) []mqttPublication {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var matched []mqttPublication
		for _, publication := range client.publicationsSnapshot() {
			if strings.HasPrefix(publication.topic, prefix+"/") || publication.topic == prefix || strings.HasPrefix(publication.topic, prefix) {
				matched = append(matched, publication)
			}
		}
		if len(matched) > 0 {
			return matched
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

func TestMQTTRunLoopDrainsDirtyStateAfterSpuriousWake(t *testing.T) {
	client := &recordingMQTTClient{}
	service := NewMQTTService(noopCommandService{}, MQTTConfig{
		TopicPrefix:       "growatt/spf5000es",
		HADiscoveryPrefix: "homeassistant",
		HADeviceID:        "growatt_spf5000es",
		OperationTimeout:  time.Second,
		DisconnectTimeout: time.Second,
	})
	service.client = client
	service.connected = true

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go service.Run(ctx)

	service.StatusUpdated(map[string]any{"PV1Volt": 1.0})
	if pubs := waitForTopicPublications(t, client, "growatt/spf5000es/status", time.Second); len(pubs) == 0 {
		t.Fatal("expected initial status publication")
	}
	client.clearPublications()

	service.mu.Lock()
	service.pendingStatus = map[string]any{"PV1Volt": 2.0}
	service.statusDirty = true
	service.updatesPending = false
	service.mu.Unlock()
	service.updateWake <- struct{}{}

	if pubs := waitForTopicPublications(t, client, "growatt/spf5000es/status", time.Second); len(pubs) == 0 {
		t.Fatal("mqtt run loop failed to drain dirty state after consuming a spurious wake")
	}
}

func TestMQTTRepublishesDirtyStateAfterDeferredConnect(t *testing.T) {
	client := &recordingMQTTClient{}
	pollingService := NewPollingService(&fakePollInverter{
		status: map[string]any{"PV1Volt": 380.5},
		config: map[string]any{"BatteryType": "Lithium"},
	}, nil, PollingConfig{
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

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() {
		if err := mqttService.Run(ctx); err != nil && err != context.Canceled {
			t.Errorf("mqtt loop stopped: %v", err)
		}
	}()

	mqttService.StatusUpdated(map[string]any{"PV1Volt": 380.5})
	time.Sleep(20 * time.Millisecond)

	mqttService.mu.Lock()
	mqttService.connectionID++
	connectionID := mqttService.connectionID
	mqttService.mu.Unlock()
	mqttService.onConnect(client, connectionID)

	publications := waitForTopicPublications(t, client, "growatt/spf5000es", 2*time.Second)
	if len(publications) == 0 {
		t.Fatalf("expected republication after connect, got none (%d total publishes)", len(client.publicationsSnapshot()))
	}
}

func TestPollingToMQTTPublishesWhenUpdatesArriveBeforeMQTTRun(t *testing.T) {
	client := &recordingMQTTClient{}
	inverter := &fakePollInverter{
		status: map[string]any{"PV1Volt": 380.5},
		config: map[string]any{"BatteryType": "Lithium"},
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

	go func() {
		if err := pollingService.Run(ctx); err != nil && err != context.Canceled {
			t.Errorf("polling loop stopped: %v", err)
		}
	}()

	mqttService.mu.Lock()
	mqttService.connectionID++
	connectionID := mqttService.connectionID
	mqttService.mu.Unlock()
	mqttService.onConnect(client, connectionID)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		mqttService.mu.Lock()
		dirty := mqttService.statusDirty || mqttService.configDirty
		mqttService.mu.Unlock()
		if dirty {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	go func() {
		if err := mqttService.Run(ctx); err != nil && err != context.Canceled {
			t.Errorf("mqtt loop stopped: %v", err)
		}
	}()

	publications := waitForTopicPublications(t, client, "growatt/spf5000es", 2*time.Second)
	if len(publications) == 0 {
		t.Fatalf("expected MQTT publications after mqtt run started, got none (%d total publishes)", len(client.publicationsSnapshot()))
	}
}

type gatedPollInverter struct {
	fakePollInverter
	statusRelease <-chan struct{}
}

func (i *gatedPollInverter) ReadStatus(ctx context.Context) (map[string]any, error) {
	if i.statusRelease != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-i.statusRelease:
		}
	}
	return i.fakePollInverter.ReadStatus(ctx)
}

func TestPollingToMQTTPublishesAfterInFlightStatusDuringDisconnect(t *testing.T) {
	client := &recordingMQTTClient{}
	statusRelease := make(chan struct{})
	inverter := &gatedPollInverter{
		fakePollInverter: fakePollInverter{
			status: map[string]any{"PV1Volt": 380.5},
			config: map[string]any{"BatteryType": "Lithium"},
		},
		statusRelease: statusRelease,
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

	if pubs := waitForTopicPublications(t, client, "growatt/spf5000es/config", 2*time.Second); len(pubs) == 0 {
		t.Fatal("expected config publication before disconnect race")
	}
	client.clearPublications()

	mqttService.onConnectionLost()
	close(statusRelease)
	time.Sleep(50 * time.Millisecond)

	mqttService.mu.Lock()
	mqttService.connectionID++
	connectionID = mqttService.connectionID
	mqttService.mu.Unlock()
	mqttService.onConnect(client, connectionID)

	publications := waitForTopicPublications(t, client, "growatt/spf5000es", 2*time.Second)
	if len(publications) == 0 {
		t.Fatalf("expected MQTT republication after reconnect, got none (%d total publishes)", len(client.publicationsSnapshot()))
	}
}

func TestPollingToMQTTPublishesAfterConnect(t *testing.T) {
	client := &recordingMQTTClient{}
	inverter := &fakePollInverter{
		status: map[string]any{"PV1Volt": 380.5},
		config: map[string]any{"BatteryType": "Lithium"},
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
	connectionID := mqttService.connectionID
	mqttService.mu.Unlock()
	mqttService.onConnect(client, connectionID)

	publications := waitForTopicPublications(t, client, "growatt/spf5000es", 2*time.Second)
	if len(publications) == 0 {
		t.Fatalf("expected MQTT publications under growatt/spf5000es, got none (%d total publishes)", len(client.publicationsSnapshot()))
	}

	var hasStatus, hasConfig bool
	for _, publication := range publications {
		switch {
		case strings.Contains(publication.topic, "/status/"):
			hasStatus = true
		case strings.Contains(publication.topic, "/config/"):
			hasConfig = true
		}
	}
	if !hasStatus {
		t.Fatalf("expected status publication, got topics: %v", publicationTopics(publications))
	}
	if !hasConfig {
		t.Fatalf("expected config publication, got topics: %v", publicationTopics(publications))
	}
}

func publicationTopics(publications []mqttPublication) []string {
	topics := make([]string, len(publications))
	for index, publication := range publications {
		topics[index] = publication.topic
	}
	return topics
}
