package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/paho"
)

func TestMQTTTransportSchemeAndTLSMinimum(t *testing.T) {
	plain := NewMQTTService(nil, MQTTConfig{Host: "broker", Port: 1883})
	if got := plain.clientConfig.ServerUrls[0].Scheme; got != "mqtt" {
		t.Fatalf("plain MQTT scheme = %q, want mqtt", got)
	}

	secureConfig := MQTTConfig{Host: "broker", Port: 8883, TLSEnabled: true}
	secure := NewMQTTService(nil, secureConfig)
	if got := secure.clientConfig.ServerUrls[0].Scheme; got != "tls" {
		t.Fatalf("secure MQTT scheme = %q, want tls", got)
	}
	tlsConfig, err := mqttTLSConfig(secureConfig)
	if err != nil {
		t.Fatal(err)
	}
	if tlsConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("minimum TLS version = %d, want TLS 1.2", tlsConfig.MinVersion)
	}
}

type mqttPublication struct {
	topic   string
	payload []byte
	retain  bool
}

type recordingMQTTClient struct {
	open            bool
	publications    []mqttPublication
	subscriptions   []paho.SubscribeOptions
	subscribeErrs   []error
	disconnects     int
	publishDeadline bool
}

func (c *recordingMQTTClient) Disconnect(context.Context) error {
	c.open = false
	c.disconnects++
	return nil
}
func (c *recordingMQTTClient) Subscribe(_ context.Context, subscribe *paho.Subscribe) (*paho.Suback, error) {
	c.subscriptions = append(c.subscriptions, subscribe.Subscriptions...)
	if len(c.subscribeErrs) == 0 {
		return &paho.Suback{}, nil
	}
	err := c.subscribeErrs[0]
	c.subscribeErrs = c.subscribeErrs[1:]
	return &paho.Suback{}, err
}
func (c *recordingMQTTClient) Publish(ctx context.Context, publish *paho.Publish) (*paho.PublishResponse, error) {
	_, c.publishDeadline = ctx.Deadline()
	c.publications = append(c.publications, mqttPublication{
		topic: publish.Topic, payload: append([]byte(nil), publish.Payload...), retain: publish.Retain,
	})
	return &paho.PublishResponse{}, nil
}

func TestMQTTPublishesWithDeadline(t *testing.T) {
	client := &recordingMQTTClient{}
	service := NewMQTTService(nil, MQTTConfig{
		TopicPrefix: "inverter", HADiscoveryPrefix: "homeassistant", HADeviceID: "test_inverter",
		OperationTimeout: time.Second,
	})
	service.client = client
	service.publish("inverter/status", []byte("ready"), true)

	if !client.publishDeadline {
		t.Fatal("MQTT publish context has no deadline")
	}
}

type errUnexpectedPayloadType struct{ payload any }

func (e errUnexpectedPayloadType) Error() string { return "unexpected MQTT payload type" }

func TestMQTTValue(t *testing.T) {
	tests := []struct {
		value any
		want  string
	}{
		{value: "online", want: "online"},
		{value: int64(-1234), want: "-1234"},
		{value: 23.4, want: "23.4"},
		{value: true, want: "true"},
		{value: false, want: "false"},
	}
	for _, test := range tests {
		if got := string(mqttValue(test.value)); got != test.want {
			t.Errorf("mqttValue(%v) = %q, want %q", test.value, got, test.want)
		}
	}
}

func TestParseMQTTCommandValuePreservesUsefulScalarTypes(t *testing.T) {
	tests := []struct {
		payload string
		want    any
	}{
		{payload: "ON", want: true},
		{payload: "false", want: false},
		{payload: `"PV First"`, want: "PV First"},
		{payload: "42", want: float64(42)},
		{payload: "52.4", want: float64(52.4)},
		{payload: "unquoted text", want: "unquoted text"},
	}
	for _, test := range tests {
		t.Run(test.payload, func(t *testing.T) {
			got, err := parseMQTTCommandValue(test.payload)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("parseMQTTCommandValue(%q) = %#v, want %#v", test.payload, got, test.want)
			}
		})
	}

	for _, payload := range []string{"NaN", "+Inf"} {
		if _, err := parseMQTTCommandValue(payload); err == nil {
			t.Errorf("parseMQTTCommandValue(%q) accepted a non-finite value", payload)
		}
	}
}

func TestMQTTKeepsLatestUpdateWhileDisconnected(t *testing.T) {
	client := &recordingMQTTClient{}
	service := &MQTTService{
		client:       client,
		availability: "inverter/availability",
		statusTopics: map[string]string{"PV1Volt": "inverter/status/pv1_volt/state"},
		configTopics: make(map[string]string),
		updateWake:   make(chan struct{}, 1),
	}
	status := map[string]any{"PV1Volt": 123.4}
	service.StatusUpdated(status)

	if !service.runPending() {
		t.Fatal("status update was not marked pending")
	}
	if len(client.publications) != 0 {
		t.Fatalf("published %d messages while disconnected", len(client.publications))
	}
	service.mu.Lock()
	retained := service.statusDirty && service.pendingStatus != nil
	service.connected = true
	service.mu.Unlock()
	if !retained {
		t.Fatal("disconnected status update was discarded")
	}

	service.requestUpdatePublish()
	if !service.runPending() {
		t.Fatal("reconnect did not schedule the retained update")
	}
	if len(client.publications) != 1 {
		t.Fatalf("published %d messages after reconnect, want 1", len(client.publications))
	}
	if got := string(client.publications[0].payload); got != "123.4" {
		t.Fatalf("status payload = %q, want 123.4", got)
	}
	if !client.publications[0].retain {
		t.Fatal("status payload was not retained")
	}
}

func TestMQTTDiscoveryDoesNotRetainCommands(t *testing.T) {
	client := &recordingMQTTClient{}
	service := NewMQTTService(nil, MQTTConfig{
		TopicPrefix: "inverter", HADiscoveryPrefix: "homeassistant", HADeviceID: "test_inverter",
	})
	service.client = client
	service.publishDeviceDiscovery()

	if len(client.publications) != 1 {
		t.Fatalf("published %d discovery messages, want 1", len(client.publications))
	}
	var discovery struct {
		Components map[string]map[string]any `json:"components"`
	}
	if err := json.Unmarshal(client.publications[0].payload, &discovery); err != nil {
		t.Fatal(err)
	}
	for objectID, component := range discovery.Components {
		if _, hasCommand := component["command_topic"]; !hasCommand {
			continue
		}
		if retain, ok := component["retain"].(bool); !ok || retain {
			t.Errorf("command component %q retain = %#v, want false", objectID, component["retain"])
		}
		if qos, ok := component["qos"].(float64); !ok || qos != 1 {
			t.Errorf("command component %q qos = %#v, want 1", objectID, component["qos"])
		}
		if objectID != "test_inverter_sync_time" {
			if optimistic, ok := component["optimistic"].(bool); !ok || !optimistic {
				t.Errorf("command component %q optimistic = %#v, want true", objectID, component["optimistic"])
			}
		}
	}
}

type noopCommandService struct{}

func (noopCommandService) QueueConfigWrite(string, any) error { return nil }
func (noopCommandService) SetPollingEnabled(bool)             {}
func (noopCommandService) RequestTimeSync() error             { return nil }

func TestMQTTSubscribesToCommandsAtQoS1(t *testing.T) {
	client := &recordingMQTTClient{}
	service := NewMQTTService(noopCommandService{}, MQTTConfig{
		TopicPrefix: "inverter", HADiscoveryPrefix: "homeassistant", HADeviceID: "test_inverter",
		OperationTimeout: time.Second, DisconnectTimeout: time.Second,
	})
	service.onConnect(client, 0)

	if len(client.subscriptions) != 2 {
		t.Fatalf("subscriptions = %d, want 2", len(client.subscriptions))
	}
	for _, subscription := range client.subscriptions {
		if subscription.QoS != 1 {
			t.Errorf("subscription %q QoS = %d, want 1", subscription.Topic, subscription.QoS)
		}
	}
}

func TestMQTTRetriesCommandSubscriptions(t *testing.T) {
	client := &recordingMQTTClient{subscribeErrs: []error{
		errors.New("temporary subscription failure"),
		errors.New("another temporary subscription failure"),
	}}
	service := NewMQTTService(noopCommandService{}, MQTTConfig{
		TopicPrefix: "inverter", HADiscoveryPrefix: "homeassistant", HADeviceID: "test_inverter",
		OperationTimeout: time.Second, DisconnectTimeout: time.Second,
	})
	service.sleep = func(context.Context, time.Duration) error { return nil }
	service.onConnect(client, 0)

	if got := len(client.subscriptions); got != 6 {
		t.Fatalf("subscription entries = %d, want 6", got)
	}
	if client.disconnects != 0 {
		t.Fatalf("disconnects = %d, want 0", client.disconnects)
	}
}

func BenchmarkMQTTStatusHandoff(b *testing.B) {
	service := &MQTTService{updateWake: make(chan struct{}, 1)}
	status := make(map[string]any, len(inputRegisters))
	for index, definition := range inputRegisters {
		status[definition.Key] = int64(index + 1000)
	}
	b.ReportAllocs()
	for b.Loop() {
		service.StatusUpdated(status)
	}
}
