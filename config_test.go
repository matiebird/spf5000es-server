package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTextLogHandlerOmitsTimestamp(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(newTextLogHandler(&output, slog.LevelInfo))

	logger.InfoContext(context.Background(), "service started", "revision", "abc123")

	got := output.String()
	if strings.Contains(got, "time=") {
		t.Fatalf("log contains timestamp: %q", got)
	}
	if !strings.Contains(got, "level=INFO msg=\"service started\" revision=abc123") {
		t.Fatalf("log is missing expected fields: %q", got)
	}
}

func TestReadConfigAppliesDefaultsAndNormalizesTopics(t *testing.T) {
	path := writeTestConfig(t, `
[MODBUS]
PORT = /dev/ttyUSB0

[MQTT]
HA_DEVICE_ID = test_inverter
USER = none
PASSWORD = false
TOPIC_PREFIX = /solar/inverter/

[LOGGING]
LEVEL = warning
`)

	got, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Modbus.Port != "/dev/ttyUSB0" || got.Modbus.Timeout != 1500*time.Millisecond {
		t.Fatalf("Modbus config = %#v", got.Modbus)
	}
	if got.MQTT.Host != "localhost" || got.MQTT.Port != 1883 || got.MQTT.ClientID != "test_inverter" {
		t.Fatalf("MQTT defaults = %#v", got.MQTT)
	}
	if got.MQTT.WillDelay != 5*time.Minute {
		t.Fatalf("MQTT will delay = %v, want 5m", got.MQTT.WillDelay)
	}
	if got.MQTT.OperationTimeout != 10*time.Second {
		t.Fatalf("MQTT operation timeout = %v, want 10s", got.MQTT.OperationTimeout)
	}
	if got.Recovery.ReconnectAttempts != 3 || got.Recovery.InitialBackoff != time.Second ||
		got.Recovery.MaxBackoff != 10*time.Second || got.Recovery.ResetCooldown != 5*time.Minute {
		t.Fatalf("recovery defaults = %#v", got.Recovery)
	}
	if got.MQTT.Username != "" || got.MQTT.Password != "" {
		t.Fatalf("optional credentials were not cleared: user=%q password=%q", got.MQTT.Username, got.MQTT.Password)
	}
	if got.MQTT.TopicPrefix != "solar/inverter" || got.MQTT.HADiscoveryPrefix != "homeassistant" {
		t.Fatalf("normalized topics = %q, %q", got.MQTT.TopicPrefix, got.MQTT.HADiscoveryPrefix)
	}
	if got.LogLevel != slog.LevelWarn {
		t.Fatalf("log level = %v, want WARN", got.LogLevel)
	}
}

func TestReadConfigRejectsMissingPortAndInvalidLogLevel(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{name: "missing Modbus port", config: "[MQTT]\nHOST=localhost\n"},
		{name: "invalid log level", config: "[MODBUS]\nPORT=/dev/ttyUSB0\n[LOGGING]\nLEVEL=verbose\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := readConfig(writeTestConfig(t, test.config)); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
}

func TestReadConfigRejectsInvalidValues(t *testing.T) {
	tests := []struct{ name, setting string }{
		{"unknown section", "[BROKER]\nHOST=localhost"},
		{"unknown key", "[MQTT]\nBROKR_PORT=1883"},
		{"key outside section", "BROKER_PORT=1883"},
		{"invalid port", "[MQTT]\nPORT=abc"},
		{"port too low", "[MQTT]\nPORT=0"},
		{"port too high", "[MQTT]\nPORT=65536"},
		{"empty port", "[MQTT]\nPORT="},
		{"invalid duration", "[POLLING]\nSTATUS_INTERVAL_SEC=soon"},
		{"non-finite duration", "[POLLING]\nSTATUS_INTERVAL_SEC=NaN"},
		{"non-positive duration", "[POLLING]\nSTATUS_INTERVAL_SEC=0"},
		{"short Modbus timeout", "[MODBUS]\nTIMEOUT_SEC=0.01"},
		{"no reconnect attempts", "[RECOVERY]\nRECONNECT_ATTEMPTS=0"},
		{"too many reconnect attempts", "[RECOVERY]\nRECONNECT_ATTEMPTS=21"},
		{"backoff maximum below initial", "[RECOVERY]\nINITIAL_BACKOFF_SEC=5\nMAX_BACKOFF_SEC=1"},
		{"short keepalive", "[MQTT]\nKEEPALIVE_SEC=0.5"},
		{"short MQTT operation timeout", "[MQTT]\nOPERATION_TIMEOUT_SEC=0.01"},
		{"short config interval", "[POLLING]\nCONFIG_INTERVAL_SEC=0.5"},
		{"empty host", "[MQTT]\nHOST="},
		{"wildcard topic", "[MQTT]\nTOPIC_PREFIX=solar/+"},
		{"empty normalized topic", "[MQTT]\nTOPIC_PREFIX=///"},
		{"path device ID", "[MQTT]\nHA_DEVICE_ID=solar/inverter"},
		{"control character client ID", "[MQTT]\nCLIENT_ID=bad\x01id"},
		{"invalid TLS boolean", "[MQTT]\nTLS_ENABLED=perhaps"},
		{"TLS options without TLS", "[MQTT]\nTLS_CA_FILE=ca.pem"},
		{"TLS certificate without key", "[MQTT]\nTLS_ENABLED=true\nTLS_CERT_FILE=client.pem"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := "[MODBUS]\nPORT=/dev/ttyUSB0\n" + test.setting + "\n"
			if _, err := readConfig(writeTestConfig(t, config)); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
}

func writeTestConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.ini")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
