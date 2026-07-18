package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

type registerType uint8

const (
	registerUint registerType = iota
	registerInt
	registerChar
)

type rawRegisterValue struct {
	integer int64
	text    string
	isText  bool
}

func (v rawRegisterValue) value() any {
	if v.isText {
		return v.text
	}
	return v.integer
}

func (v rawRegisterValue) asInt64() (int64, error) {
	if v.isText {
		return asInt64(v.text)
	}
	return v.integer, nil
}

func (v rawRegisterValue) asFloat64() (float64, error) {
	if v.isText {
		return asFloat64(v.text)
	}
	return float64(v.integer), nil
}

type readValueFunc func(rawRegisterValue) (any, error)
type writeValueFunc func(any) (any, error)

type registerDef struct {
	Key    string
	Start  int
	Length int
	Type   registerType
	Read   readValueFunc
	Write  writeValueFunc
}

type registerWindow struct {
	Start int
	Count int
}

func identity(value rawRegisterValue) (any, error) { return value.value(), nil }

func asBool(value rawRegisterValue) (any, error) {
	number, err := value.asInt64()
	if err != nil {
		return nil, err
	}
	return number != 0, nil
}

func boolWrite(value any) (any, error) {
	parsed, err := parseBool(value)
	if err != nil {
		return nil, err
	}
	if parsed {
		return int64(1), nil
	}
	return int64(0), nil
}

func scaled(divisor float64) readValueFunc {
	return func(value rawRegisterValue) (any, error) {
		number, err := value.asFloat64()
		if err != nil {
			return nil, err
		}
		return number / divisor, nil
	}
}

func scaleWrite(multiplier float64) writeValueFunc {
	return func(value any) (any, error) {
		number, err := asFloat64(value)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return nil, fmt.Errorf("invalid numeric value")
		}
		return int64(math.Round(number * multiplier)), nil
	}
}

func intWrite(value any) (any, error) {
	return asInt64(value)
}

func stringWrite(value any) (any, error) {
	text, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("value must be a string")
	}
	return text, nil
}

func enumRead(values map[int64]any) readValueFunc {
	return func(value rawRegisterValue) (any, error) {
		number, err := value.asInt64()
		if err != nil {
			return nil, err
		}
		decoded, ok := values[number]
		if !ok {
			return nil, fmt.Errorf("unknown enum value %d", number)
		}
		return decoded, nil
	}
}

func enumWrite(values map[string]int64) writeValueFunc {
	return func(value any) (any, error) {
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("value must be a string")
		}
		encoded, ok := values[text]
		if !ok {
			return nil, fmt.Errorf("unknown enum value %q", text)
		}
		return encoded, nil
	}
}

func systemStatus(value rawRegisterValue) (any, error) {
	number, err := value.asInt64()
	if err != nil {
		return nil, err
	}
	if number >= 0 && number < int64(len(systemStatuses)) {
		return systemStatuses[number], nil
	}
	return fmt.Sprintf("Unknown (%d)", number), nil
}

var systemStatuses = [...]any{
	"Standby", "PV&Grid Supporting Loads", "Battery Discharging",
	"Fault", "Flash", "PV Charging", "Grid Charging",
	"PV&Grid Charging", "PV&Grid Charging+Grid Bypass",
	"PV Charging+Grid Bypass", "Grid Charging+Grid Bypass",
	"Grid Bypass", "PV Charging+Loads Supporting", "PV Discharging",
	"PV&Battery Discharging", "Gen Charging", "Gen Charging+Gen Bypass",
	"PV&Gen Charging", "PV&Gen Charging+Gen Bypass",
	"PV Charging+Gen Bypass", "Gen Bypass", "PV Export to Grid",
	"PV Export to Grid+Loads Supporting", "PV Charging+Export to Grid",
	"PV Charging+Export to Grid+Loads Supporting", "Battery Export to Grid",
	"Battery Export to Grid+Loads Supporting", "Battery&PV Export to Grid",
	"Battery&PV Export to Grid+Loads Supporting",
}

var inputRegisters = []registerDef{
	{"SystemStatus", 0, 1, registerUint, systemStatus, nil},
	{"PV1Volt", 1, 1, registerUint, scaled(10), nil},
	{"PV2Volt", 2, 1, registerUint, scaled(10), nil},
	{"PV1Watt", 3, 2, registerUint, scaled(10), nil},
	{"PV2Watt", 5, 2, registerUint, scaled(10), nil},
	{"PV1Amps", 7, 1, registerUint, scaled(10), nil},
	{"PV2Amps", 8, 1, registerUint, scaled(10), nil},
	{"OutputWatt", 9, 2, registerUint, scaled(10), nil},
	{"OutputVA", 11, 2, registerUint, scaled(10), nil},
	{"ACChrWatt", 13, 2, registerUint, scaled(10), nil},
	{"ACChrVA", 15, 2, registerUint, scaled(10), nil},
	{"BatteryVolt", 17, 1, registerUint, scaled(100), nil},
	{"BatterySOC", 18, 1, registerUint, identity, nil},
	{"BusVolt", 19, 1, registerUint, scaled(10), nil},
	{"GridVolt", 20, 1, registerUint, scaled(10), nil},
	{"LineFreq", 21, 1, registerUint, scaled(100), nil},
	{"OutputACVolt", 22, 1, registerUint, scaled(10), nil},
	{"OutputACFreq", 23, 1, registerUint, scaled(100), nil},
	{"OutputDCVolt", 24, 1, registerUint, scaled(10), nil},
	{"InvTempC", 25, 1, registerInt, scaled(10), nil},
	{"DCDCTempC", 26, 1, registerInt, scaled(10), nil},
	{"LoadPercent", 27, 1, registerUint, scaled(10), nil},
	{"BatteryPortVolt", 28, 1, registerUint, scaled(100), nil},
	{"BatteryBusVolt", 29, 1, registerUint, scaled(100), nil},
	{"WorkTimeTotalSeconds", 30, 2, registerUint, scaled(2), nil},
	{"Buck1TempC", 32, 1, registerInt, scaled(10), nil},
	{"Buck2TempC", 33, 1, registerInt, scaled(10), nil},
	{"OutputAmps", 34, 1, registerUint, scaled(10), nil},
	{"InvAmps", 35, 1, registerUint, scaled(10), nil},
	{"ACInputWatt", 36, 2, registerInt, scaled(10), nil},
	{"ACInputVA", 38, 2, registerUint, scaled(10), nil},
	{"FaultBit", 40, 1, registerUint, identity, nil},
	{"WarningBit", 41, 1, registerUint, identity, nil},
	{"WarningBitHigh", 42, 1, registerUint, identity, nil},
	{"WarningValue", 43, 1, registerUint, identity, nil},
	{"DeviceTypeCode", 44, 1, registerUint, identity, nil},
	{"ExportToGridTodaykWh", 45, 1, registerUint, scaled(10), nil},
	{"ExportToGridTotalkWh", 46, 2, registerUint, scaled(10), nil},
	{"PV1EnergyTodaykWh", 48, 2, registerUint, scaled(10), nil},
	{"PV1EnergyTotalkWh", 50, 2, registerUint, scaled(10), nil},
	{"PV2EnergyTodaykWh", 52, 2, registerUint, scaled(10), nil},
	{"PV2EnergyTotalkWh", 54, 2, registerUint, scaled(10), nil},
	{"ACChargeEnergyTodaykWh", 56, 2, registerUint, scaled(10), nil},
	{"ACChargeEnergyTotalkWh", 58, 2, registerUint, scaled(10), nil},
	{"BatteryDischargeEnergyTodaykWh", 60, 2, registerUint, scaled(10), nil},
	{"BatteryDischargeEnergyTotalkWh", 62, 2, registerUint, scaled(10), nil},
	{"ACDischargeEnergyTodaykWh", 64, 2, registerUint, scaled(10), nil},
	{"ACDischargeEnergyTotalkWh", 66, 2, registerUint, scaled(10), nil},
	{"ACChargeBatteryAmps", 68, 1, registerUint, scaled(10), nil},
	{"ACDischargeWatt", 69, 2, registerUint, scaled(10), nil},
	{"ACDischargeVA", 71, 2, registerUint, scaled(10), nil},
	{"BatteryDischargeWatt", 73, 2, registerUint, scaled(10), nil},
	{"BatteryDischargeVA", 75, 2, registerUint, scaled(10), nil},
	{"BatteryWatt", 77, 2, registerInt, scaled(10), nil},
	{"SlaveExistCount", 79, 1, registerUint, identity, nil},
	{"MpptFanSpeedPercent", 81, 1, registerUint, identity, nil},
	{"InvFanSpeedPercent", 82, 1, registerUint, identity, nil},
	{"TotalChargeAmps", 83, 1, registerUint, scaled(10), nil},
	{"TotalDischargeAmps", 84, 1, registerUint, scaled(10), nil},
	{"OPDischargeEnergyTodaykWh", 85, 2, registerUint, scaled(10), nil},
	{"OPDischargeEnergyTotalkWh", 87, 2, registerUint, scaled(10), nil},
}

var (
	outputConfig   = map[int64]any{0: "SBU", 1: "SOL", 2: "UTI", 3: "SUB"}
	chargeConfig   = map[int64]any{0: "PV First", 1: "PV&UTI", 2: "PV Only"}
	pvModel        = map[int64]any{0: "Independent", 1: "Parallel"}
	acInModel      = map[int64]any{0: "APL", 1: "UPS", 2: "GEN"}
	outputVoltType = map[int64]any{
		0: "208VAC", 1: "230VAC", 2: "240VAC", 3: "220VAC",
		4: "100VAC", 5: "110VAC", 6: "120VAC",
	}
	outputFreqType  = map[int64]any{0: "50Hz", 1: "60Hz"}
	overLoadRestart = map[int64]any{0: "Yes", 1: "No", 2: "Switch to UTI"}
	overTempRestart = map[int64]any{0: true, 1: false}
	batteryType     = map[int64]any{0: "AGM", 1: "FLD", 2: "USE", 3: "Lithium", 4: "USE2"}
	agingMode       = map[int64]any{0: "Normal", 1: "Aging"}
	safetyType      = map[int64]any{1: "Standard", 2: "ETL", 3: "AS4777", 4: "CQC", 5: "VDE4105"}
	onOff           = map[int64]any{0x0000: "Output enable", 0x0100: "Output disable"}
)

func reverseEnum(values map[int64]any) map[string]int64 {
	reversed := make(map[string]int64, len(values))
	for key, value := range values {
		if text, ok := value.(string); ok {
			reversed[text] = key
		}
	}
	return reversed
}

func overTempWrite(value any) (any, error) {
	parsed, err := parseBool(value)
	if err != nil {
		return nil, err
	}
	if parsed {
		return int64(0), nil
	}
	return int64(1), nil
}

var holdingRegisters = []registerDef{
	{"OnOff", 0, 1, registerUint, enumRead(onOff), nil},
	{"OutputConfig", 1, 1, registerUint, enumRead(outputConfig), enumWrite(reverseEnum(outputConfig))},
	{"ChargeConfig", 2, 1, registerUint, enumRead(chargeConfig), enumWrite(reverseEnum(chargeConfig))},
	{"UtiOutStart", 3, 1, registerUint, identity, intWrite},
	{"UtiOutEnd", 4, 1, registerUint, identity, intWrite},
	{"UtiChargeStart", 5, 1, registerUint, identity, intWrite},
	{"UtiChargeEnd", 6, 1, registerUint, identity, intWrite},
	{"PVModel", 7, 1, registerUint, enumRead(pvModel), enumWrite(reverseEnum(pvModel))},
	{"ACInModel", 8, 1, registerUint, enumRead(acInModel), enumWrite(reverseEnum(acInModel))},
	{"FWVersion", 9, 3, registerChar, identity, nil},
	{"FWVersion2", 12, 3, registerChar, identity, nil},
	{"LCDLanguage", 15, 1, registerUint, identity, intWrite},
	{"GridV_Adj", 16, 1, registerUint, identity, nil},
	{"InvV_Adj", 17, 1, registerUint, identity, nil},
	{"OutputVoltType", 18, 1, registerUint, enumRead(outputVoltType), enumWrite(reverseEnum(outputVoltType))},
	{"OutputFreqType", 19, 1, registerUint, enumRead(outputFreqType), enumWrite(reverseEnum(outputFreqType))},
	{"OverLoadRestart", 20, 1, registerUint, enumRead(overLoadRestart), enumWrite(reverseEnum(overLoadRestart))},
	{"OverTempRestart", 21, 1, registerUint, enumRead(overTempRestart), overTempWrite},
	{"BuzzerEnable", 22, 1, registerUint, asBool, boolWrite},
	{"SerialNumber", 23, 5, registerChar, identity, stringWrite},
	{"MoudleH", 28, 1, registerUint, identity, intWrite},
	{"MoudleL", 29, 1, registerUint, identity, intWrite},
	{"ComAddress", 30, 1, registerUint, identity, intWrite},
	{"FlashStart", 31, 1, registerUint, identity, intWrite},
	{"ResetUserInfo", 32, 1, registerUint, identity, intWrite},
	{"ResetToFactory", 33, 1, registerUint, identity, intWrite},
	{"MaxChargeAmps", 34, 1, registerUint, identity, intWrite},
	{"BulkChargeVolt", 35, 1, registerUint, scaled(10), scaleWrite(10)},
	{"FloatChargeVolt", 36, 1, registerUint, scaled(10), scaleWrite(10)},
	{"BatLowtoUti", 37, 1, registerUint, scaled(10), scaleWrite(10)},
	{"ACChargeAmps", 38, 1, registerUint, identity, intWrite},
	{"BatteryType", 39, 1, registerUint, enumRead(batteryType), enumWrite(reverseEnum(batteryType))},
	{"AgingMode", 40, 1, registerUint, enumRead(agingMode), enumWrite(reverseEnum(agingMode))},
	{"FunctionMask", 41, 1, registerUint, identity, intWrite},
	{"SafetyType", 42, 1, registerUint, enumRead(safetyType), enumWrite(reverseEnum(safetyType))},
	{"DTC", 43, 1, registerUint, identity, nil},
	{"HoldingChipSelect", 51, 1, registerUint, identity, nil},
	{"uwAcVHighL", 52, 1, registerUint, identity, nil},
	{"uwAcVLowL", 53, 1, registerUint, identity, nil},
	{"uwAcFreqHighL", 54, 1, registerUint, identity, nil},
	{"uwAcFreqLowL", 55, 1, registerUint, identity, nil},
	{"HoldingVar1Setting", 56, 1, registerUint, identity, nil},
	{"DebugModeEnable", 57, 1, registerUint, asBool, nil},
	{"ControlFWBuildNo2", 67, 1, registerUint, identity, nil},
	{"ControlFWBuildNo1", 68, 1, registerUint, identity, nil},
	{"ComFWBuildNo2", 69, 1, registerUint, identity, nil},
	{"ComFWBuildNo1", 70, 1, registerUint, identity, nil},
	{"ModbusVersion", 73, 1, registerUint, identity, nil},
	{"SCCComMode", 75, 1, registerUint, identity, nil},
	{"RateWatt", 76, 2, registerUint, scaled(10), nil},
	{"RateVA", 78, 2, registerUint, scaled(10), nil},
	{"ComboardVer", 80, 1, registerUint, identity, nil},
	{"uwBatPieceNum", 81, 1, registerUint, identity, intWrite},
	{"wBatLowCutOff", 82, 1, registerUint, scaled(10), nil},
	{"MaxGeneratorChargeAmps", 83, 1, registerUint, identity, nil},
	{"NomGridVRaw", 84, 1, registerUint, identity, nil},
	{"NomGridFreqRaw", 85, 1, registerUint, identity, nil},
	{"NomBatVRaw", 86, 1, registerUint, identity, nil},
	{"NomPVCurrRaw", 87, 1, registerUint, identity, nil},
	{"NomAcChgCurrRaw", 88, 1, registerUint, identity, nil},
	{"NomOpVRaw", 89, 1, registerUint, identity, nil},
	{"NomOpFreqRaw", 90, 1, registerUint, identity, nil},
	{"NomOpPowRaw", 91, 1, registerUint, identity, nil},
	{"uwAC2BatVolt", 95, 1, registerUint, scaled(10), scaleWrite(10)},
	{"BypEnable", 96, 1, registerUint, asBool, boolWrite},
	{"PowSavingEnable", 97, 1, registerUint, asBool, boolWrite},
	{"SpowBalEnable", 98, 1, registerUint, asBool, boolWrite},
	{"ClrEnergyToday", 99, 1, registerUint, asBool, boolWrite},
	{"ClrEnergyAll", 100, 1, registerUint, asBool, boolWrite},
	{"BurnInTestEnable", 101, 1, registerUint, asBool, boolWrite},
	{"ManualStartEnable", 102, 1, registerUint, asBool, boolWrite},
	{"SciLossChkEnable", 103, 1, registerUint, asBool, boolWrite},
	{"BlightEnable", 104, 1, registerUint, asBool, boolWrite},
	{"ParaMaxChgAmps", 105, 1, registerUint, identity, nil},
	{"LiProtocolType", 106, 1, registerUint, identity, intWrite},
	{"AudioAlarmEnable", 107, 1, registerUint, asBool, boolWrite},
}

var (
	inputRegisterWindow    = buildRegisterWindow(inputRegisters)
	holdingRegisterWindows = buildRegisterWindows(holdingRegisters, maxHoldingReadRegisters)
	holdingByKey           = registerMap(holdingRegisters)
)

func registerMap(registers []registerDef) map[string]registerDef {
	result := make(map[string]registerDef, len(registers))
	for _, definition := range registers {
		result[definition.Key] = definition
	}
	return result
}

func buildRegisterWindow(registers []registerDef) registerWindow {
	if len(registers) == 0 {
		return registerWindow{}
	}
	start := registers[0].Start
	end := start + registers[0].Length - 1
	for _, definition := range registers[1:] {
		definitionEnd := definition.Start + definition.Length - 1
		if definition.Start < start {
			start = definition.Start
		}
		if definitionEnd > end {
			end = definitionEnd
		}
	}
	return registerWindow{Start: start, Count: end - start + 1}
}

func buildRegisterWindows(registers []registerDef, maximum int) []registerWindow {
	window := buildRegisterWindow(registers)
	if window.Count == 0 {
		return nil
	}
	var windows []registerWindow
	end := window.Start + window.Count - 1
	for current := window.Start; current <= end; current += maximum {
		count := maximum
		if current+count-1 > end {
			count = end - current + 1
		}
		windows = append(windows, registerWindow{Start: current, Count: count})
	}
	return windows
}

func asInt64(value any) (int64, error) {
	switch typed := value.(type) {
	case int:
		return int64(typed), nil
	case int64:
		return typed, nil
	case uint16:
		return int64(typed), nil
	case float64:
		return floatAsInt64(typed)
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0, err
		}
		return floatAsInt64(parsed)
	default:
		return 0, fmt.Errorf("unsupported numeric value %T", value)
	}
}

func floatAsInt64(value float64) (int64, error) {
	rounded := math.Round(value)
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value-rounded) >= 1e-3 {
		return 0, fmt.Errorf("value is not an integer")
	}
	return int64(rounded), nil
}

func asFloat64(value any) (float64, error) {
	switch typed := value.(type) {
	case int:
		return float64(typed), nil
	case int64:
		return float64(typed), nil
	case uint16:
		return float64(typed), nil
	case float64:
		return typed, nil
	case string:
		return strconv.ParseFloat(strings.TrimSpace(typed), 64)
	default:
		return 0, fmt.Errorf("unsupported numeric value %T", value)
	}
}

func parseBool(value any) (bool, error) {
	switch typed := value.(type) {
	case bool:
		return typed, nil
	case int:
		return typed != 0, nil
	case int64:
		return typed != 0, nil
	case float64:
		return typed != 0, nil
	case string:
		text := strings.TrimSpace(typed)
		switch {
		case strings.EqualFold(text, "yes"), strings.EqualFold(text, "true"),
			strings.EqualFold(text, "t"), text == "1", strings.EqualFold(text, "on"):
			return true, nil
		case strings.EqualFold(text, "no"), strings.EqualFold(text, "false"),
			strings.EqualFold(text, "f"), text == "0", strings.EqualFold(text, "off"):
			return false, nil
		}
	}
	return false, fmt.Errorf("boolean value expected")
}

var selectOptions = map[string][]string{
	"OutputConfig":    {"SBU", "SOL", "UTI", "SUB"},
	"ChargeConfig":    {"PV First", "PV&UTI", "PV Only"},
	"PVModel":         {"Independent", "Parallel"},
	"ACInModel":       {"APL", "UPS", "GEN"},
	"OutputVoltType":  {"208VAC", "230VAC", "240VAC", "220VAC", "100VAC", "110VAC", "120VAC"},
	"OutputFreqType":  {"50Hz", "60Hz"},
	"OverLoadRestart": {"Yes", "No", "Switch to UTI"},
	"BatteryType":     {"AGM", "FLD", "USE", "Lithium", "USE2"},
	"AgingMode":       {"Normal", "Aging"},
	"SafetyType":      {"Standard", "ETL", "AS4777", "CQC", "VDE4105"},
}

var booleanKeys = map[string]bool{
	"OverTempRestart": true, "BuzzerEnable": true, "BypEnable": true,
	"PowSavingEnable": true, "SpowBalEnable": true, "ClrEnergyToday": true,
	"ClrEnergyAll": true, "BurnInTestEnable": true, "ManualStartEnable": true,
	"SciLossChkEnable": true, "BlightEnable": true, "AudioAlarmEnable": true,
}

type numberLimits struct {
	Min  float64
	Max  float64
	Step float64
}

var configNumberLimits = map[string]numberLimits{
	"UtiOutStart": {0, 23, 1}, "UtiOutEnd": {0, 23, 1},
	"UtiChargeStart": {0, 23, 1}, "UtiChargeEnd": {0, 23, 1},
	"LCDLanguage": {0, 1, 1}, "MoudleH": {0, 1, 1},
	"ComAddress": {1, 254, 1}, "ResetUserInfo": {0, 1, 1},
	"ResetToFactory": {0, 1, 1}, "MaxChargeAmps": {0, 180, 1},
	"BulkChargeVolt": {50, 64, 0.1}, "FloatChargeVolt": {50, 56, 0.1},
	"ACChargeAmps":   {0, 80, 1},
	"LiProtocolType": {1, 99, 1},
}

var entityMetadata = map[string]map[string]any{
	"SystemStatus":         {"icon": "mdi:solar-power"},
	"FaultBit":             {"icon": "mdi:alert-circle-outline"},
	"WarningBit":           {"icon": "mdi:alert-outline"},
	"WarningBitHigh":       {"icon": "mdi:alert-outline"},
	"WarningValue":         {"icon": "mdi:alert-outline"},
	"DeviceTypeCode":       {"icon": "mdi:identifier"},
	"WorkTimeTotalSeconds": {"icon": "mdi:timer-outline"},
	"OutputConfig":         {"icon": "mdi:transmission-tower-export"},
	"ChargeConfig":         {"icon": "mdi:battery-charging"},
	"UtiOutStart":          {"icon": "mdi:clock-start", "unit_of_measurement": "h"},
	"UtiOutEnd":            {"icon": "mdi:clock-end", "unit_of_measurement": "h"},
	"UtiChargeStart":       {"icon": "mdi:battery-clock", "unit_of_measurement": "h"},
	"UtiChargeEnd":         {"icon": "mdi:battery-clock", "unit_of_measurement": "h"},
	"PVModel":              {"icon": "mdi:solar-panel"}, "ACInModel": {"icon": "mdi:transmission-tower-import"},
	"FWVersion": {"icon": "mdi:chip"}, "FWVersion2": {"icon": "mdi:chip"},
	"LCDLanguage": {"icon": "mdi:translate"}, "SerialNumber": {"icon": "mdi:barcode"},
	"MoudleH": {"icon": "mdi:chip"}, "MoudleL": {"icon": "mdi:chip"},
	"ComAddress": {"icon": "mdi:serial-port"}, "FlashStart": {"icon": "mdi:flash"},
	"ResetUserInfo": {"icon": "mdi:account-sync-outline"}, "ResetToFactory": {"icon": "mdi:factory"},
	"BatteryType": {"icon": "mdi:car-battery"}, "AgingMode": {"icon": "mdi:timer-sand"},
	"FunctionMask": {"icon": "mdi:bitwise"}, "SafetyType": {"icon": "mdi:shield-check-outline"},
	"DTC":               {"icon": "mdi:alert-decagram-outline"},
	"ControlFWBuildNo2": {"icon": "mdi:chip"}, "ControlFWBuildNo1": {"icon": "mdi:chip"},
	"ComFWBuildNo2": {"icon": "mdi:chip"}, "ComFWBuildNo1": {"icon": "mdi:chip"},
	"ModbusVersion": {"icon": "mdi:protocol"}, "SCCComMode": {"icon": "mdi:connection"},
	"ComboardVer": {"icon": "mdi:chip"}, "uwBatPieceNum": {"icon": "mdi:battery-multiple"},
	"uwAC2BatVolt": {"name": "uw AC2 Bat"}, "LiProtocolType": {"icon": "mdi:protocol"},
}
