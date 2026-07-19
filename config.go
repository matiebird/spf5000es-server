package main

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/ini.v1"
)

type ModbusConfig struct {
	Port    string
	Timeout time.Duration
}

type MQTTConfig struct {
	Host              string
	Port              int
	Username          string
	Password          string
	ClientID          string
	Keepalive         time.Duration
	WillDelay         time.Duration
	DisconnectTimeout time.Duration
	TopicPrefix       string
	HADiscoveryPrefix string
	HADeviceID        string
	HADeviceName      string
}

type PollingConfig struct {
	ConfigInterval time.Duration
	StatusInterval time.Duration
}

type AppConfig struct {
	Modbus   ModbusConfig
	MQTT     MQTTConfig
	Polling  PollingConfig
	LogLevel slog.Level
}

func logConfig(logger *slog.Logger, c AppConfig) {
	setting := func(name string, value any) {
		logger.Info("config", "setting", name, "value", value)
	}

	setting("MODBUS.PORT", c.Modbus.Port)
	setting("MODBUS.TIMEOUT_SEC", c.Modbus.Timeout.Seconds())
	setting("MQTT.HOST", c.MQTT.Host)
	setting("MQTT.PORT", c.MQTT.Port)
	setting("MQTT.USER", c.MQTT.Username)
	password := "not configured"
	if c.MQTT.Password != "" {
		password = "configured"
	}
	setting("MQTT.PASSWORD", password)
	setting("MQTT.CLIENT_ID", c.MQTT.ClientID)
	setting("MQTT.KEEPALIVE_SEC", c.MQTT.Keepalive.Seconds())
	setting("MQTT.WILL_DELAY_SEC", c.MQTT.WillDelay.Seconds())
	setting("MQTT.DISCONNECT_TIMEOUT_SEC", c.MQTT.DisconnectTimeout.Seconds())
	setting("MQTT.TOPIC_PREFIX", c.MQTT.TopicPrefix)
	setting("MQTT.HA_DISCOVERY_PREFIX", c.MQTT.HADiscoveryPrefix)
	setting("MQTT.HA_DEVICE_ID", c.MQTT.HADeviceID)
	setting("MQTT.HA_DEVICE_NAME", c.MQTT.HADeviceName)
	setting("POLLING.CONFIG_INTERVAL_SEC", c.Polling.ConfigInterval.Seconds())
	setting("POLLING.STATUS_INTERVAL_SEC", c.Polling.StatusInterval.Seconds())
	setting("LOGGING.LEVEL", c.LogLevel.String())
}

func readConfig(path string) (AppConfig, error) {
	cfg, err := ini.Load(path)
	if err != nil {
		return AppConfig{}, fmt.Errorf("read config %q: %w", path, err)
	}
	if err := validateConfigSchema(cfg); err != nil {
		return AppConfig{}, err
	}
	if err := validateExplicitValues(cfg); err != nil {
		return AppConfig{}, err
	}

	modbusSection := cfg.Section("MODBUS")
	port := strings.TrimSpace(modbusSection.Key("PORT").String())
	if port == "" {
		return AppConfig{}, fmt.Errorf("MODBUS.PORT is required")
	}

	modbusTimeout, err := durationSeconds(cfg, "MODBUS", "TIMEOUT_SEC", 1.5)
	if err != nil {
		return AppConfig{}, err
	}
	mqttPort, err := intValue(cfg, "MQTT", "PORT", 1883)
	if err != nil {
		return AppConfig{}, err
	}
	keepalive, err := durationSeconds(cfg, "MQTT", "KEEPALIVE_SEC", 60)
	if err != nil {
		return AppConfig{}, err
	}
	willDelay, err := durationSeconds(cfg, "MQTT", "WILL_DELAY_SEC", 300)
	if err != nil {
		return AppConfig{}, err
	}
	disconnectTimeout, err := durationSeconds(cfg, "MQTT", "DISCONNECT_TIMEOUT_SEC", 5)
	if err != nil {
		return AppConfig{}, err
	}
	configInterval, err := durationSeconds(cfg, "POLLING", "CONFIG_INTERVAL_SEC", 600)
	if err != nil {
		return AppConfig{}, err
	}
	statusInterval, err := durationSeconds(cfg, "POLLING", "STATUS_INTERVAL_SEC", 2)
	if err != nil {
		return AppConfig{}, err
	}

	haDeviceID := value(cfg, "MQTT", "HA_DEVICE_ID", "growatt_spf5000es")
	app := AppConfig{
		Modbus: ModbusConfig{
			Port:    port,
			Timeout: modbusTimeout,
		},
		MQTT: MQTTConfig{
			Host:              value(cfg, "MQTT", "HOST", "localhost"),
			Port:              mqttPort,
			Username:          optionalValue(cfg, "MQTT", "USER"),
			Password:          optionalValue(cfg, "MQTT", "PASSWORD"),
			ClientID:          value(cfg, "MQTT", "CLIENT_ID", haDeviceID),
			Keepalive:         keepalive,
			WillDelay:         willDelay,
			DisconnectTimeout: disconnectTimeout,
			TopicPrefix:       normalizeTopic(value(cfg, "MQTT", "TOPIC_PREFIX", haDeviceID), haDeviceID),
			HADiscoveryPrefix: normalizeTopic(value(cfg, "MQTT", "HA_DISCOVERY_PREFIX", "homeassistant"), "homeassistant"),
			HADeviceID:        haDeviceID,
			HADeviceName:      value(cfg, "MQTT", "HA_DEVICE_NAME", "Growatt SPF 5000 ES"),
		},
		Polling: PollingConfig{
			ConfigInterval: configInterval,
			StatusInterval: statusInterval,
		},
	}

	if app.Modbus.Timeout < 100*time.Millisecond {
		return AppConfig{}, fmt.Errorf("MODBUS.TIMEOUT_SEC must be at least 0.1")
	}
	if app.MQTT.Keepalive < time.Second {
		return AppConfig{}, fmt.Errorf("MQTT.KEEPALIVE_SEC must be at least 1")
	}
	if app.MQTT.Keepalive > 65535*time.Second {
		return AppConfig{}, fmt.Errorf("MQTT.KEEPALIVE_SEC must not exceed 65535")
	}
	if app.MQTT.WillDelay < time.Second || app.MQTT.WillDelay > time.Duration(math.MaxUint32-1)*time.Second {
		return AppConfig{}, fmt.Errorf("MQTT.WILL_DELAY_SEC must be between 1 and %d", uint64(math.MaxUint32-1))
	}
	if app.Polling.ConfigInterval < time.Second {
		return AppConfig{}, fmt.Errorf("POLLING.CONFIG_INTERVAL_SEC must be at least 1")
	}
	if app.Polling.StatusInterval < time.Second {
		return AppConfig{}, fmt.Errorf("POLLING.STATUS_INTERVAL_SEC must be at least 1")
	}
	if app.MQTT.Port < 1 || app.MQTT.Port > 65535 {
		return AppConfig{}, fmt.Errorf("MQTT.PORT must be between 1 and 65535")
	}
	if err := validateMQTTIdentifier("MQTT.CLIENT_ID", app.MQTT.ClientID, false); err != nil {
		return AppConfig{}, err
	}
	if err := validateMQTTIdentifier("MQTT.HA_DEVICE_ID", app.MQTT.HADeviceID, true); err != nil {
		return AppConfig{}, err
	}
	for name, topic := range map[string]string{
		"MQTT.TOPIC_PREFIX":        app.MQTT.TopicPrefix,
		"MQTT.HA_DISCOVERY_PREFIX": app.MQTT.HADiscoveryPrefix,
	} {
		if err := validateMQTTTopic(name, topic); err != nil {
			return AppConfig{}, err
		}
	}

	level, err := parseLogLevel(value(cfg, "LOGGING", "LEVEL", "INFO"))
	if err != nil {
		return AppConfig{}, err
	}
	app.LogLevel = level
	return app, nil
}

func value(cfg *ini.File, section, key, fallback string) string {
	raw := strings.TrimSpace(cfg.Section(section).Key(key).String())
	if raw == "" {
		return fallback
	}
	return raw
}

func optionalValue(cfg *ini.File, section, key string) string {
	raw := strings.TrimSpace(cfg.Section(section).Key(key).String())
	switch strings.ToLower(raw) {
	case "", "none", "null", "false":
		return ""
	default:
		return raw
	}
}

func intValue(cfg *ini.File, section, key string, fallback int) (int, error) {
	entry, err := cfg.Section(section).GetKey(key)
	if err != nil {
		return fallback, nil
	}
	raw := strings.TrimSpace(entry.String())
	if raw == "" {
		return 0, fmt.Errorf("%s.%s must not be empty", section, key)
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s.%s %q: %w", section, key, raw, err)
	}
	return parsed, nil
}

func durationSeconds(cfg *ini.File, section, key string, fallback float64) (time.Duration, error) {
	entry, err := cfg.Section(section).GetKey(key)
	if err != nil {
		return time.Duration(fallback * float64(time.Second)), nil
	}
	raw := strings.TrimSpace(entry.String())
	if raw == "" {
		return 0, fmt.Errorf("%s.%s must not be empty", section, key)
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 {
		return 0, fmt.Errorf("%s.%s must be a finite positive number, got %q", section, key, raw)
	}
	if parsed > float64(math.MaxInt64)/float64(time.Second) {
		return 0, fmt.Errorf("%s.%s is too large", section, key)
	}
	return time.Duration(parsed * float64(time.Second)), nil
}

var configSchema = map[string]map[string]struct{}{
	"MODBUS":  keys("PORT", "TIMEOUT_SEC"),
	"MQTT":    keys("HOST", "PORT", "USER", "PASSWORD", "CLIENT_ID", "KEEPALIVE_SEC", "WILL_DELAY_SEC", "DISCONNECT_TIMEOUT_SEC", "TOPIC_PREFIX", "HA_DISCOVERY_PREFIX", "HA_DEVICE_ID", "HA_DEVICE_NAME"),
	"POLLING": keys("CONFIG_INTERVAL_SEC", "STATUS_INTERVAL_SEC"),
	"LOGGING": keys("LEVEL"),
}

func keys(names ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(names))
	for _, name := range names {
		result[name] = struct{}{}
	}
	return result
}

func validateConfigSchema(cfg *ini.File) error {
	for _, section := range cfg.Sections() {
		if section.Name() == ini.DEFAULT_SECTION {
			if len(section.Keys()) != 0 {
				return fmt.Errorf("configuration keys must be inside a named section")
			}
			continue
		}
		allowed, ok := configSchema[section.Name()]
		if !ok {
			return fmt.Errorf("unknown configuration section %q", section.Name())
		}
		for _, key := range section.Keys() {
			if _, ok := allowed[key.Name()]; !ok {
				return fmt.Errorf("unknown configuration key %s.%s", section.Name(), key.Name())
			}
		}
	}
	return nil
}

func validateExplicitValues(cfg *ini.File) error {
	for _, field := range [][2]string{
		{"MODBUS", "PORT"}, {"MQTT", "HOST"}, {"MQTT", "CLIENT_ID"},
		{"MQTT", "TOPIC_PREFIX"}, {"MQTT", "HA_DISCOVERY_PREFIX"},
		{"MQTT", "HA_DEVICE_ID"}, {"MQTT", "HA_DEVICE_NAME"},
		{"LOGGING", "LEVEL"},
	} {
		section, key := field[0], field[1]
		if cfg.Section(section).HasKey(key) && strings.TrimSpace(cfg.Section(section).Key(key).String()) == "" {
			return fmt.Errorf("%s.%s must not be empty", section, key)
		}
	}
	for _, key := range []string{"TOPIC_PREFIX", "HA_DISCOVERY_PREFIX"} {
		if cfg.Section("MQTT").HasKey(key) && strings.Trim(strings.TrimSpace(cfg.Section("MQTT").Key(key).String()), "/") == "" {
			return fmt.Errorf("MQTT.%s must contain a topic name", key)
		}
	}
	return nil
}

func validateMQTTIdentifier(name, value string, pathSafe bool) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	for _, character := range value {
		if character == 0 || character < 0x20 || character == 0x7f || character == '+' || character == '#' || (pathSafe && character == '/') {
			return fmt.Errorf("%s contains an invalid character", name)
		}
	}
	return nil
}

func validateMQTTTopic(name, topic string) error {
	if topic == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	for _, character := range topic {
		if character == 0 || character < 0x20 || character == 0x7f || character == '+' || character == '#' {
			return fmt.Errorf("%s must be a valid MQTT topic without wildcards", name)
		}
	}
	return nil
}

func normalizeTopic(topic, fallback string) string {
	topic = strings.Trim(topic, "/")
	if topic == "" {
		return fallback
	}
	return topic
}

func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "DEBUG":
		return slog.LevelDebug, nil
	case "INFO":
		return slog.LevelInfo, nil
	case "WARN", "WARNING":
		return slog.LevelWarn, nil
	case "ERROR":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid logging level %q", raw)
	}
}

func configureLogging(level slog.Level) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})))
}
