package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/eclipse/paho.golang/paho"
)

type mqttPublication struct {
	topic   string
	payload []byte
	retain  bool
}

type recordingMQTTClient struct {
	open         bool
	publications []mqttPublication
}

func (c *recordingMQTTClient) Disconnect(context.Context) error {
	c.open = false
	return nil
}
func (c *recordingMQTTClient) Subscribe(context.Context, *paho.Subscribe) (*paho.Suback, error) {
	return &paho.Suback{}, nil
}
func (c *recordingMQTTClient) Publish(_ context.Context, publish *paho.Publish) (*paho.PublishResponse, error) {
	c.publications = append(c.publications, mqttPublication{
		topic: publish.Topic, payload: append([]byte(nil), publish.Payload...), retain: publish.Retain,
	})
	return &paho.PublishResponse{}, nil
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
