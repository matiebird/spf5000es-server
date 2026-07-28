package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
)

var (
	mqttOnlinePayload  = []byte("online")
	mqttOfflinePayload = []byte("offline")
	mqttTruePayload    = []byte("true")
	mqttFalsePayload   = []byte("false")
)

const (
	mqttSubscribeInitialBackoff = time.Second
	mqttSubscribeMaxBackoff     = 30 * time.Second
)

type mqttClient interface {
	Subscribe(context.Context, *paho.Subscribe) (*paho.Suback, error)
	Publish(context.Context, *paho.Publish) (*paho.PublishResponse, error)
	Disconnect(context.Context) error
}

type MQTTService struct {
	mu                     sync.Mutex
	commands               inverterCommandService
	config                 MQTTConfig
	client                 mqttClient
	clientConfig           autopaho.ClientConfig
	connected              bool
	initialConfigPublished bool
	initialStatusPublished bool
	discoveryPending       bool
	onlinePublished        bool
	batteryType            string
	pendingStatus          map[string]any
	pendingConfig          map[string]any
	statusDirty            bool
	configDirty            bool
	updatesPending         bool
	slugToConfigKey        map[string]string
	base                   string
	availability           string
	timeSyncCommand        string
	configPrefix           string
	configFilter           string
	statusTopics           map[string]string
	configTopics           map[string]string
	updateWake             chan struct{}
	connectionID           uint64
	sleep                  func(context.Context, time.Duration) error
}

func NewMQTTService(commands inverterCommandService, config MQTTConfig) *MQTTService {
	service := &MQTTService{
		commands: commands, config: config,
		slugToConfigKey: make(map[string]string, len(holdingRegisters)),
		statusTopics:    make(map[string]string, len(inputRegisters)),
		configTopics:    make(map[string]string, len(holdingRegisters)),
		updateWake:      make(chan struct{}, 1),
		sleep:           sleepContext,
	}
	service.base = strings.Trim(config.TopicPrefix, "/")
	service.availability = service.base + "/availability"
	service.timeSyncCommand = service.base + "/time_sync/set"
	service.configPrefix = service.base + "/config/"
	service.configFilter = service.configPrefix + "+/set"
	for _, definition := range inputRegisters {
		service.statusTopics[definition.Key] = valueTopic(service.base, "status", definition.Key)
	}
	for _, definition := range holdingRegisters {
		keySlug := slug(definition.Key)
		service.slugToConfigKey[keySlug] = definition.Key
		service.configTopics[definition.Key] = service.base + "/config/" + keySlug + "/state"
	}

	willDelay := uint32(config.WillDelay / time.Second)
	sessionExpiry := max(uint32(60), willDelay+1)
	scheme := "mqtt"
	if config.TLSEnabled {
		scheme = "tls"
	}
	service.clientConfig = autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{{Scheme: scheme, Host: fmt.Sprintf("%s:%d", config.Host, config.Port)}},
		KeepAlive:                     uint16(config.Keepalive / time.Second),
		CleanStartOnInitialConnection: false,
		SessionExpiryInterval:         sessionExpiry,
		ReconnectBackoff:              autopaho.NewExponentialBackoff(time.Second, 30*time.Second, time.Second, 2),
		ConnectUsername:               config.Username,
		ConnectPassword:               []byte(config.Password),
		WillMessage: &paho.WillMessage{
			Topic: service.availabilityTopic(), Payload: mqttOfflinePayload, QoS: 0, Retain: true,
		},
		WillProperties: &paho.WillProperties{WillDelayInterval: &willDelay},
		DisconnectPacketBuilder: func() *paho.Disconnect {
			return &paho.Disconnect{ReasonCode: packets.DisconnectDisconnectWithWillMessage}
		},
		ClientConfig: paho.ClientConfig{
			ClientID: config.ClientID,
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){func(received paho.PublishReceived) (bool, error) {
				service.onMessage(received.Packet.Topic, received.Packet.Payload)
				return true, nil
			}},
			OnClientError: func(err error) { slog.Warn("MQTT client error", "error", err) },
		},
	}
	service.clientConfig.OnConnectionUp = func(client *autopaho.ConnectionManager, _ *paho.Connack) {
		service.mu.Lock()
		service.connectionID++
		connectionID := service.connectionID
		service.mu.Unlock()
		go service.onConnect(client, connectionID)
	}
	service.clientConfig.OnConnectionDown = func() bool {
		service.onConnectionLost()
		return true
	}
	service.clientConfig.OnConnectError = func(err error) {
		slog.Error("MQTT connection attempt failed; reconnect remains enabled", "error", err)
	}

	return service
}

func (s *MQTTService) Start() error {
	if s.config.TLSEnabled {
		tlsConfig, err := mqttTLSConfig(s.config)
		if err != nil {
			return err
		}
		s.clientConfig.TlsCfg = tlsConfig
	}
	slog.Info("connecting MQTT broker", "host", s.config.Host, "port", s.config.Port, "tls", s.config.TLSEnabled, "client_id", s.config.ClientID)
	client, err := autopaho.NewConnection(context.Background(), s.clientConfig)
	if err != nil {
		return fmt.Errorf("start MQTT client: %w", err)
	}
	s.mu.Lock()
	s.client = client
	s.mu.Unlock()
	return nil
}

func mqttTLSConfig(config MQTTConfig) (*tls.Config, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: config.TLSServerName}
	if config.TLSCAFile != "" {
		contents, err := os.ReadFile(config.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("read MQTT TLS CA file: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(contents) {
			return nil, fmt.Errorf("MQTT.TLS_CA_FILE contains no valid PEM certificates")
		}
		tlsConfig.RootCAs = roots
	}
	if config.TLSCertFile != "" {
		certificate, err := tls.LoadX509KeyPair(config.TLSCertFile, config.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load MQTT TLS client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	return tlsConfig, nil
}

func (s *MQTTService) Run(ctx context.Context) error {
	for {
		if s.runPending() {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.updateWake:
		}
	}
}

func (s *MQTTService) Stop() {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), s.config.DisconnectTimeout)
		defer cancel()
		if err := client.Disconnect(ctx); err != nil {
			slog.Warn("MQTT disconnect failed", "error", err)
		}
	}
}

func (s *MQTTService) baseTopic() string {
	return s.base
}

func (s *MQTTService) availabilityTopic() string {
	return s.availability
}

func (s *MQTTService) onConnect(client mqttClient, connectionID uint64) {
	s.mu.Lock()
	if connectionID != s.connectionID {
		s.mu.Unlock()
		return
	}
	s.client = client
	s.mu.Unlock()
	slog.Info("MQTT connected")

	if err := s.subscribeCommands(client, connectionID); err != nil {
		slog.Warn("MQTT connection setup stopped", "error", err)
		return
	}

	s.mu.Lock()
	if connectionID != s.connectionID {
		s.mu.Unlock()
		return
	}
	s.connected = true
	s.initialConfigPublished = false
	s.initialStatusPublished = false
	s.onlinePublished = false
	s.commands.SetPollingEnabled(true)
	s.mu.Unlock()
	s.requestUpdatePublish()
}

func (s *MQTTService) subscribeCommands(client mqttClient, connectionID uint64) error {
	subscribe := &paho.Subscribe{Subscriptions: []paho.SubscribeOptions{
		{Topic: s.configFilter, QoS: 1},
		{Topic: s.timeSyncCommand, QoS: 1},
	}}
	delay := mqttSubscribeInitialBackoff
	for attempt := 1; ; attempt++ {
		s.mu.Lock()
		current := connectionID == s.connectionID
		s.mu.Unlock()
		if !current {
			return fmt.Errorf("MQTT connection changed during setup")
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.config.OperationTimeout)
		_, subscribeErr := client.Subscribe(ctx, subscribe)
		cancel()
		if subscribeErr == nil {
			return nil
		}
		slog.Warn("MQTT command subscription failed; retrying", "attempt", attempt, "retry_in", delay, "error", subscribeErr)
		if err := s.sleep(context.Background(), delay); err != nil {
			return err
		}
		delay = min(delay*2, mqttSubscribeMaxBackoff)
	}
}

func (s *MQTTService) onConnectionLost() {
	s.mu.Lock()
	s.connectionID++
	s.connected = false
	s.initialConfigPublished = false
	s.initialStatusPublished = false
	s.onlinePublished = false
	s.commands.SetPollingEnabled(false)
	s.mu.Unlock()
	slog.Warn("MQTT disconnected")
}

func (s *MQTTService) onMessage(topic string, body []byte) {
	payload := strings.TrimSpace(string(body))
	if err := s.handleCommand(topic, payload); err != nil {
		slog.Warn("rejected MQTT command", "topic", topic, "error", err)
	}
}

func (s *MQTTService) handleCommand(topic, payload string) error {
	if topic == s.timeSyncCommand {
		return s.commands.RequestTimeSync()
	}

	const suffix = "/set"
	if !strings.HasPrefix(topic, s.configPrefix) || !strings.HasSuffix(topic, suffix) {
		return nil
	}
	key := s.slugToConfigKey[topic[len(s.configPrefix):len(topic)-len(suffix)]]
	if key == "" {
		return fmt.Errorf("unknown config key")
	}
	value, err := parseMQTTCommandValue(payload)
	if err != nil {
		return err
	}
	return s.commands.QueueConfigWrite(key, value)
}

func parseMQTTCommandValue(payload string) (any, error) {
	switch {
	case strings.EqualFold(payload, "true"), strings.EqualFold(payload, "on"):
		return true, nil
	case strings.EqualFold(payload, "false"), strings.EqualFold(payload, "off"):
		return false, nil
	}
	var decoded any
	if json.Unmarshal([]byte(payload), &decoded) == nil {
		switch decoded.(type) {
		case string, float64, bool:
			return decoded, nil
		}
	}
	return finiteScalar(payload)
}

func (s *MQTTService) StatusUpdated(status map[string]any) {
	s.mu.Lock()
	s.pendingStatus = status
	s.statusDirty = true
	s.updatesPending = true
	s.mu.Unlock()
	slog.Debug("mqtt status update queued",
		"fields", len(status),
		"status_dirty", true,
		"updates_pending", true,
	)
	s.signalUpdatePublisher()
}

func (s *MQTTService) publishStatus(status map[string]any) {
	slog.Debug("mqtt publishing status", "fields", len(status))
	for key, value := range status {
		topic := s.statusTopics[key]
		if topic == "" {
			topic = valueTopic(s.base, "status", key)
		}
		s.publish(topic, mqttValue(value), true)
	}
}

func (s *MQTTService) ConfigUpdated(config map[string]any) {
	s.mu.Lock()
	s.pendingConfig = config
	s.configDirty = true
	s.updatesPending = true
	s.mu.Unlock()
	slog.Debug("mqtt config update queued",
		"fields", len(config),
		"config_dirty", true,
		"updates_pending", true,
	)
	s.signalUpdatePublisher()
}

func (s *MQTTService) InverterAvailabilityChanged(available bool) {
	s.mu.Lock()
	connected := s.connected
	s.onlinePublished = available && connected
	s.mu.Unlock()
	if !connected {
		return
	}
	payload := mqttOfflinePayload
	if available {
		payload = mqttOnlinePayload
	}
	s.publish(s.availabilityTopic(), payload, true)
}

func (s *MQTTService) publishConfig(config map[string]any) {
	slog.Debug("mqtt publishing config", "fields", len(config))
	for key, value := range config {
		topic := s.configTopics[key]
		if topic == "" {
			topic = valueTopic(s.base, "config", key)
		}
		s.publish(topic, mqttValue(value), true)
	}
	if battery, ok := config["BatteryType"].(string); ok {
		s.updateBatteryType(battery)
	}
}

func (s *MQTTService) requestUpdatePublish() {
	s.mu.Lock()
	s.updatesPending = true
	s.mu.Unlock()
	s.signalUpdatePublisher()
}

func (s *MQTTService) signalUpdatePublisher() {
	select {
	case s.updateWake <- struct{}{}:
	default:
	}
}

func (s *MQTTService) publishUpdates() {
	s.mu.Lock()
	if !s.connected {
		statusDirty := s.statusDirty
		configDirty := s.configDirty
		if statusDirty || configDirty {
			s.updatesPending = true
		}
		s.mu.Unlock()
		slog.Debug("mqtt publish updates deferred",
			"connected", false,
			"status_dirty", statusDirty,
			"config_dirty", configDirty,
			"updates_pending_rearmed", statusDirty || configDirty,
		)
		return
	}
	statusDirty, configDirty := s.statusDirty, s.configDirty
	status, config := s.pendingStatus, s.pendingConfig
	connectionID := s.connectionID
	s.pendingStatus, s.pendingConfig = nil, nil
	s.statusDirty, s.configDirty = false, false
	s.mu.Unlock()
	slog.Debug("mqtt publish updates starting",
		"connected", true,
		"status_dirty", statusDirty,
		"config_dirty", configDirty,
		"status_fields", len(status),
		"config_fields", len(config),
	)
	if statusDirty {
		s.publishStatus(status)
	}
	if configDirty {
		s.publishConfig(config)
	}
	s.publishReadyAfterInitialState(connectionID, configDirty, statusDirty)
}

func (s *MQTTService) publishReadyAfterInitialState(connectionID uint64, configPublished, statusPublished bool) {
	s.mu.Lock()
	if !s.connected || connectionID != s.connectionID {
		s.mu.Unlock()
		return
	}
	s.initialConfigPublished = s.initialConfigPublished || configPublished
	s.initialStatusPublished = s.initialStatusPublished || statusPublished
	if !s.initialConfigPublished || !s.initialStatusPublished {
		s.mu.Unlock()
		return
	}
	publishDiscovery := s.discoveryPending
	s.discoveryPending = false
	publishOnline := !s.onlinePublished
	s.onlinePublished = true
	initialConfigPublished := s.initialConfigPublished
	initialStatusPublished := s.initialStatusPublished
	s.mu.Unlock()
	slog.Debug("mqtt discovery decision",
		"publish_discovery", publishDiscovery,
		"publish_online", publishOnline,
		"initial_config_published", initialConfigPublished,
		"initial_status_published", initialStatusPublished,
	)
	if publishDiscovery {
		s.publishDeviceDiscovery()
	}
	if publishOnline {
		s.publish(s.availabilityTopic(), mqttOnlinePayload, true)
	}
}

func (s *MQTTService) runPending() bool {
	s.mu.Lock()
	updates := s.updatesPending
	pendingDirty := s.statusDirty || s.configDirty
	statusDirty := s.statusDirty
	configDirty := s.configDirty
	connected := s.connected
	shouldPublish := updates || pendingDirty
	if shouldPublish {
		s.updatesPending = false
	}
	s.mu.Unlock()

	slog.Debug("mqtt run pending publish decision",
		"connected", connected,
		"updates_pending", updates,
		"status_dirty", statusDirty,
		"config_dirty", configDirty,
		"should_publish", shouldPublish,
	)

	if shouldPublish {
		s.publishUpdates()
	}
	return shouldPublish
}

func (s *MQTTService) publishDeviceDiscovery() {
	components := make(map[string]any, len(inputRegisters)+len(holdingRegisters)+1)
	for _, definition := range inputRegisters {
		objectID := s.config.HADeviceID + "_" + slug(definition.Key)
		payload := s.componentBase(objectID, friendlyName(definition.Key))
		payload["state_topic"] = s.statusTopics[definition.Key]
		merge(payload, sensorMetadata(definition.Key))
		components[objectID] = deviceComponent("sensor", payload)
	}
	for _, definition := range holdingRegisters {
		component, payload := s.configDiscovery(definition)
		objectID := payload["object_id"].(string)
		components[objectID] = deviceComponent(component, payload)
	}
	objectID := s.config.HADeviceID + "_sync_time"
	payload := s.componentBase(objectID, "Sync Time")
	payload["command_topic"] = s.timeSyncCommand
	payload["payload_press"] = "sync"
	payload["qos"] = 1
	payload["retain"] = false
	payload["icon"] = "mdi:clock-sync-outline"
	components[objectID] = deviceComponent("button", payload)

	discovery := map[string]any{
		"availability_topic": s.availabilityTopic(),
		"device": map[string]any{
			"identifiers": []string{s.config.HADeviceID}, "name": s.config.HADeviceName,
			"manufacturer": "Growatt", "model": "SPF 5000 ES",
		},
		"origin": map[string]any{
			"name": "spf5000es-server", "support_url": "https://github.com/rany2/spf5000es-server",
		},
		"components": components,
	}
	s.publishDiscoveryPayload("device", s.config.HADeviceID, discovery)
}

func deviceComponent(platform string, payload map[string]any) map[string]any {
	payload["platform"] = platform
	return payload
}

func (s *MQTTService) configDiscovery(definition registerDef) (string, map[string]any) {
	objectID := s.config.HADeviceID + "_" + slug(definition.Key)
	name := friendlyName(definition.Key)
	if metadataName, ok := entityMetadata[definition.Key]["name"].(string); ok {
		name = metadataName
	}
	payload := s.componentBase(objectID, name)
	payload["state_topic"] = s.configTopics[definition.Key]
	writable := definition.Write != nil
	if writable {
		payload["command_topic"] = s.baseTopic() + "/config/" + slug(definition.Key) + "/set"
		payload["retain"] = false
		payload["qos"] = 1
	}

	if definition.Read != nil && isBooleanRegister(definition.Key) {
		payload["payload_on"] = "true"
		payload["payload_off"] = "false"
		payload["icon"] = "mdi:toggle-switch-outline"
		if !writable {
			return "binary_sensor", payload
		}
		payload["state_on"] = "true"
		payload["state_off"] = "false"
		payload["optimistic"] = true
		return "switch", payload
	}
	if !writable {
		merge(payload, sensorMetadata(definition.Key))
		return "sensor", payload
	}
	if options, ok := selectOptions[definition.Key]; ok {
		merge(payload, sensorMetadata(definition.Key))
		payload["options"] = options
		payload["optimistic"] = true
		return "select", payload
	}
	if booleanKeys[definition.Key] {
		merge(payload, sensorMetadata(definition.Key))
		payload["payload_on"] = "true"
		payload["payload_off"] = "false"
		payload["state_on"] = "true"
		payload["state_off"] = "false"
		payload["optimistic"] = true
		return "switch", payload
	}
	if definition.Type == registerChar {
		merge(payload, sensorMetadata(definition.Key))
		payload["optimistic"] = true
		return "text", payload
	}

	merge(payload, sensorMetadata(definition.Key))
	limits := s.numberLimits(definition.Key)
	payload["min"], payload["max"], payload["step"] = limits.Min, limits.Max, limits.Step
	if definition.Key == "BatLowtoUti" || definition.Key == "uwAC2BatVolt" {
		delete(payload, "device_class")
		delete(payload, "unit_of_measurement")
		delete(payload, "state_class")
		merge(payload, s.batteryUnitMetadata())
	}
	payload["mode"] = "box"
	payload["optimistic"] = true
	return "number", payload
}

func isBooleanRegister(key string) bool {
	return key == "DebugModeEnable" || booleanKeys[key]
}

func (s *MQTTService) numberLimits(key string) numberLimits {
	if key == "BatLowtoUti" || key == "uwAC2BatVolt" {
		s.mu.Lock()
		battery := s.batteryType
		s.mu.Unlock()
		if battery == "Lithium" {
			return numberLimits{5, 100, 1}
		}
		if battery != "" {
			return numberLimits{20, 64, 0.1}
		}
		return numberLimits{5, 100, 0.1}
	}
	if limits, ok := configNumberLimits[key]; ok {
		return limits
	}
	return numberLimits{0, 65535, 1}
}

func (s *MQTTService) batteryUnitMetadata() map[string]any {
	s.mu.Lock()
	battery := s.batteryType
	s.mu.Unlock()
	switch battery {
	case "":
		return nil
	case "Lithium":
		return map[string]any{
			"icon": "mdi:percent-outline", "unit_of_measurement": "%", "state_class": "measurement",
		}
	default:
		return map[string]any{
			"device_class": "voltage", "icon": "mdi:sine-wave",
			"unit_of_measurement": "V", "state_class": "measurement",
		}
	}
}

func (s *MQTTService) updateBatteryType(battery string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if battery == "" || battery == s.batteryType {
		return
	}
	s.batteryType = battery
	s.discoveryPending = true
}

func (s *MQTTService) componentBase(objectID, name string) map[string]any {
	return map[string]any{
		"name": name, "object_id": objectID,
		"unique_id": s.config.HADeviceID + "_" + objectID,
	}
}

func (s *MQTTService) publishDiscoveryPayload(component, objectID string, payload map[string]any) {
	topic := fmt.Sprintf("%s/%s/%s/config", s.config.HADiscoveryPrefix, component, objectID)
	slog.Debug("mqtt publishing discovery", "topic", topic, "component", component, "object_id", objectID)
	if payload == nil {
		s.publish(topic, nil, true)
		return
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		slog.Error("failed to encode MQTT discovery", "object_id", objectID, "error", err)
		return
	}
	s.publish(topic, encoded, true)
}

func (s *MQTTService) publish(topic string, payload []byte, retain bool) {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		slog.Debug("mqtt publish skipped", "topic", topic, "reason", "no client")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.config.OperationTimeout)
	defer cancel()
	if _, err := client.Publish(ctx, &paho.Publish{Topic: topic, Payload: payload, QoS: 0, Retain: retain}); err != nil {
		slog.Error("MQTT publish failed", "topic", topic, "error", err)
		return
	}
	slog.Debug("mqtt published", "topic", topic, "retain", retain)
}

func merge(destination, source map[string]any) {
	for key, value := range source {
		destination[key] = value
	}
}

func mqttValue(value any) []byte {
	switch typed := value.(type) {
	case string:
		return []byte(typed)
	case int64:
		return strconv.AppendInt(nil, typed, 10)
	case float64:
		return strconv.AppendFloat(nil, typed, 'g', -1, 64)
	case bool:
		if typed {
			return mqttTruePayload
		}
		return mqttFalsePayload
	}
	return fmt.Append(nil, value)
}

func valueTopic(base, namespace, key string) string {
	return base + "/" + namespace + "/" + slug(key) + "/state"
}

var slugSeparators = regexp.MustCompile(`[^a-zA-Z0-9]+`)

func slug(value string) string {
	var output []rune
	runes := []rune(value)
	for index, current := range runes {
		if index > 0 && unicode.IsUpper(current) {
			previous := runes[index-1]
			var next rune
			if index+1 < len(runes) {
				next = runes[index+1]
			}
			if unicode.IsLower(previous) || unicode.IsDigit(previous) ||
				(unicode.IsUpper(previous) && unicode.IsLower(next)) {
				output = append(output, '_')
			}
		}
		output = append(output, unicode.ToLower(current))
	}
	return strings.Trim(slugSeparators.ReplaceAllString(string(output), "_"), "_")
}

func friendlyName(value string) string {
	var words []string
	var current []rune
	runes := []rune(value)
	for index, character := range runes {
		var previous, next rune
		if index > 0 {
			previous = runes[index-1]
		}
		if index+1 < len(runes) {
			next = runes[index+1]
		}
		boundary := len(current) > 0 && unicode.IsUpper(character) &&
			(unicode.IsLower(previous) || unicode.IsDigit(previous) ||
				(unicode.IsUpper(previous) && unicode.IsLower(next)))
		if boundary {
			words = append(words, string(current))
			current = current[:0]
		}
		current = append(current, character)
	}
	if len(current) > 0 {
		words = append(words, string(current))
	}
	return strings.Join(words, " ")
}

func sensorMetadata(key string) map[string]any {
	metadata := make(map[string]any)
	merge(metadata, entityMetadata[key])
	lower := strings.ToLower(key)

	switch {
	case strings.HasSuffix(lower, "seconds") ||
		(strings.Contains(lower, "time") && (strings.Contains(lower, "volt") || strings.Contains(lower, "freq"))):
		merge(metadata, map[string]any{"device_class": "duration", "icon": "mdi:timer-outline", "unit_of_measurement": "s"})
	case strings.HasSuffix(lower, "kwh"):
		merge(metadata, map[string]any{"device_class": "energy", "icon": "mdi:lightning-bolt", "unit_of_measurement": "kWh", "state_class": "total_increasing"})
	case strings.HasSuffix(lower, "kw"):
		merge(metadata, map[string]any{"device_class": "power", "icon": "mdi:flash", "unit_of_measurement": "kW"})
	case strings.Contains(lower, "percent") || strings.HasSuffix(lower, "soc"):
		merge(metadata, map[string]any{"icon": "mdi:percent-outline", "unit_of_measurement": "%"})
	case strings.Contains(lower, "temp") && strings.HasSuffix(lower, "c"):
		merge(metadata, map[string]any{"device_class": "temperature", "icon": "mdi:thermometer", "unit_of_measurement": "°C"})
	case strings.Contains(lower, "watt"):
		merge(metadata, map[string]any{"device_class": "power", "icon": "mdi:flash", "unit_of_measurement": "W"})
	case strings.Contains(lower, "volt"):
		merge(metadata, map[string]any{"device_class": "voltage", "icon": "mdi:sine-wave", "unit_of_measurement": "V"})
	case strings.HasSuffix(lower, "va"):
		merge(metadata, map[string]any{"device_class": "apparent_power", "icon": "mdi:flash-triangle-outline", "unit_of_measurement": "VA"})
	case strings.Contains(lower, "amps"):
		merge(metadata, map[string]any{"device_class": "current", "icon": "mdi:current-ac", "unit_of_measurement": "A"})
	case strings.Contains(lower, "freq"):
		merge(metadata, map[string]any{"device_class": "frequency", "icon": "mdi:sine-wave", "unit_of_measurement": "Hz"})
	}

	if _, hasIcon := metadata["icon"]; !hasIcon {
		switch {
		case strings.Contains(lower, "fan"):
			metadata["icon"] = "mdi:fan"
		case strings.Contains(lower, "battery") || strings.HasPrefix(lower, "bat"):
			metadata["icon"] = "mdi:battery"
		case strings.Contains(lower, "pv"):
			metadata["icon"] = "mdi:solar-panel"
		case strings.Contains(lower, "grid") || strings.Contains(lower, "uti"):
			metadata["icon"] = "mdi:transmission-tower"
		case strings.Contains(lower, "output"):
			metadata["icon"] = "mdi:power-plug-outline"
		case strings.Contains(lower, "buzzer") || strings.Contains(lower, "alarm"):
			metadata["icon"] = "mdi:bell-ring-outline"
		case strings.Contains(lower, "restart") || strings.Contains(lower, "reset"):
			metadata["icon"] = "mdi:restart"
		case strings.Contains(lower, "enable"):
			metadata["icon"] = "mdi:toggle-switch-outline"
		}
	}
	if _, hasUnit := metadata["unit_of_measurement"]; hasUnit {
		if _, hasStateClass := metadata["state_class"]; !hasStateClass && metadata["device_class"] != "duration" {
			metadata["state_class"] = "measurement"
		}
	}
	return metadata
}
