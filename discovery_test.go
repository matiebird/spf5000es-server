package main

import (
	"testing"
)

func spf5000ESLiveConfig() map[string]any {
	return map[string]any{
		"BatteryType":     "Lithium",
		"ACChargeAmps":    int64(100),
		"FloatChargeVolt": 57.0,
		"BulkChargeVolt":  57.6,
		"LiProtocolType":  int64(0),
		"MaxChargeAmps":   int64(360),
		"ParaMaxChgAmps":  int64(360),
		"NomAcChgCurrRaw": int64(300),
		"NomPVCurrRaw":    int64(600),
		"UtiOutStart":     int64(0),
		"UtiOutEnd":       int64(23),
		"UtiChargeStart":  int64(0),
		"UtiChargeEnd":    int64(23),
		"LCDLanguage":     int64(0),
		"MoudleH":         int64(0),
		"MoudleL":         int64(0),
		"ComAddress":      int64(1),
		"FlashStart":      int64(0),
		"ResetUserInfo":   int64(0),
		"ResetToFactory":  int64(0),
		"BatLowtoUti":     48.0,
		"FunctionMask":    int64(0),
		"uwBatPieceNum":   int64(1),
		"uwAC2BatVolt":    48.0,
	}
}

func TestMQTTDiscoveryNumberStatesWithinLimitsForSPF5000ESConfig(t *testing.T) {
	config := spf5000ESLiveConfig()
	service := NewMQTTService(nil, MQTTConfig{
		TopicPrefix:       "growatt/spf5000es",
		HADiscoveryPrefix: "homeassistant",
		HADeviceID:        "growatt_spf5000es",
	})
	service.mu.Lock()
	service.lastPublishedConfig = copyConfigMap(config)
	service.batteryType = "Lithium"
	service.mu.Unlock()

	for _, definition := range holdingRegisters {
		if definition.Write == nil || !isWritableNumberRegister(definition.Key) {
			continue
		}
		state, ok := config[definition.Key]
		if !ok {
			t.Fatalf("live config snapshot missing %s", definition.Key)
		}
		platform, payload := service.configDiscovery(definition)
		if platform != "number" {
			t.Fatalf("%s platform = %q, want number", definition.Key, platform)
		}
		limits := numberLimits{
			Min:  payload["min"].(float64),
			Max:  payload["max"].(float64),
			Step: payload["step"].(float64),
		}
		if !valueWithinNumberLimits(state, limits) {
			t.Fatalf("%s state %v outside discovery limits min=%v max=%v step=%v",
				definition.Key, state, limits.Min, limits.Max, limits.Step)
		}
	}
}

func TestDiscoveryNumberLimitsUseNominalRegistersForSPF5000ES(t *testing.T) {
	config := spf5000ESLiveConfig()

	acLimits := discoveryNumberLimits("ACChargeAmps", config, "Lithium")
	if acLimits.Max != 100 {
		t.Fatalf("ACChargeAmps discovery max = %v, want 100 (state value, not NomAcChgCurrRaw)", acLimits.Max)
	}

	floatLimits := discoveryNumberLimits("FloatChargeVolt", config, "Lithium")
	if floatLimits.Max != 64 {
		t.Fatalf("FloatChargeVolt discovery max = %v, want 64", floatLimits.Max)
	}

	liLimits := discoveryNumberLimits("LiProtocolType", config, "Lithium")
	if liLimits.Min != 0 {
		t.Fatalf("LiProtocolType discovery min = %v, want 0", liLimits.Min)
	}

	maxLimits := discoveryNumberLimits("MaxChargeAmps", config, "Lithium")
	if maxLimits.Max != 360 {
		t.Fatalf("MaxChargeAmps discovery max = %v, want 360 (state value within ParaMaxChgAmps)", maxLimits.Max)
	}
}

func TestDiscoveryNumberLimitsDoNotWidenWhenStateExceedsNominal(t *testing.T) {
	config := map[string]any{
		"ACChargeAmps":    int64(350),
		"NomAcChgCurrRaw": int64(300),
	}
	limits := discoveryNumberLimits("ACChargeAmps", config, "Lithium")
	if limits.Max != 80 {
		t.Fatalf("ACChargeAmps discovery max = %v, want baseline 80 when state exceeds nominal", limits.Max)
	}
}

func TestValidateWritableNumberValueRejectsBeyondNominalCeiling(t *testing.T) {
	config := spf5000ESLiveConfig()

	if err := validateWritableNumberValue("ACChargeAmps", int64(101), config, "Lithium"); err == nil {
		t.Fatal("expected ACChargeAmps above live discovery max to be rejected")
	}
	if err := validateWritableNumberValue("MaxChargeAmps", int64(361), config, "Lithium"); err == nil {
		t.Fatal("expected MaxChargeAmps above live discovery max to be rejected")
	}
	if err := validateWritableNumberValue("ACChargeAmps", int64(100), config, "Lithium"); err != nil {
		t.Fatalf("expected live ACChargeAmps to be accepted: %v", err)
	}
	if err := validateWritableNumberValue("MaxChargeAmps", int64(360), config, "Lithium"); err != nil {
		t.Fatalf("expected live MaxChargeAmps to be accepted: %v", err)
	}
}
