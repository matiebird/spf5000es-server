package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type pendingReadback struct {
	start          int
	expectedValues []uint16
}

type configCommandError struct {
	Err error
}

func (e *configCommandError) Error() string { return e.Err.Error() }
func (e *configCommandError) Unwrap() error { return e.Err }

type Inverter struct {
	mu               sync.Mutex
	client           *ModbusClient
	pendingReadbacks map[string]pendingReadback
}

func NewInverter(config ModbusConfig) *Inverter {
	return &Inverter{
		client: NewModbusClient(config),
	}
}

func (i *Inverter) Connect(ctx context.Context) error {
	slog.Info("connecting inverter")
	if err := i.client.Connect(ctx); err != nil {
		return err
	}
	slog.Info("inverter connected")
	return nil
}

func (i *Inverter) Close() error {
	return i.client.Close()
}

func (i *Inverter) SyncTime(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	var writtenValues []uint16
	if err := i.client.WriteRegisters(ctx, 45, func() ([]uint16, error) {
		now := time.Now()
		wait := time.Second - time.Duration(now.Nanosecond())
		if wait > 0 && wait < time.Second {
			if err := sleepContext(ctx, wait); err != nil {
				return nil, err
			}
			now = time.Now()
		}
		writtenValues = []uint16{
			uint16(now.Year()), uint16(now.Month()), uint16(now.Day()),
			uint16(now.Hour()), uint16(now.Minute()), uint16(now.Second()),
		}
		return writtenValues, nil
	}); err != nil {
		return err
	}
	slog.Info("inverter time synchronized", "values", writtenValues)
	return nil
}

func (i *Inverter) ReadStatus(ctx context.Context) (map[string]any, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	registers, err := readRegisterWindow(ctx, i.client.ReadInputRegisters, inputRegisterWindow)
	if err != nil {
		return nil, err
	}
	return decodeRegisterTable(registers, inputRegisters)
}

func (i *Inverter) ReadConfig(ctx context.Context) (map[string]any, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	defer clear(i.pendingReadbacks)
	registers, err := readRegisterWindows(ctx, i.client.ReadHoldingRegisters, holdingRegisterWindows)
	if err != nil {
		return nil, err
	}
	info, err := decodeRegisterTable(registers, holdingRegisters)
	if err != nil {
		return nil, err
	}
	i.reconcilePendingReadback(registers)
	return info, nil
}

func readRegisterWindow(
	ctx context.Context,
	reader func(context.Context, int, int) ([]uint16, error),
	window registerWindow,
) ([]uint16, error) {
	values, err := reader(ctx, window.Start, window.Count)
	if err != nil {
		return nil, err
	}
	if len(values) != window.Count {
		return nil, fmt.Errorf("read %d registers at %d, got %d", window.Count, window.Start, len(values))
	}
	if window.Start == 0 {
		return values, nil
	}
	registers := make([]uint16, window.Start+len(values))
	copy(registers[window.Start:], values)
	return registers, nil
}

func readRegisterWindows(
	ctx context.Context,
	reader func(context.Context, int, int) ([]uint16, error),
	windows []registerWindow,
) ([]uint16, error) {
	maxEnd := 0
	for _, window := range windows {
		if end := window.Start + window.Count; end > maxEnd {
			maxEnd = end
		}
	}
	registers := make([]uint16, maxEnd)
	for _, window := range windows {
		values, err := reader(ctx, window.Start, window.Count)
		if err != nil {
			return nil, err
		}
		if len(values) != window.Count {
			return nil, fmt.Errorf("read %d registers at %d, got %d", window.Count, window.Start, len(values))
		}
		copy(registers[window.Start:], values)
	}
	return registers, nil
}

func decodeRegisterTable(registers []uint16, definitions []registerDef) (map[string]any, error) {
	info := make(map[string]any, len(definitions))
	for _, definition := range definitions {
		raw, err := decodeRegisterValue(registers, definition)
		var value any
		if err == nil {
			value, err = definition.Read(raw)
		}
		if err != nil {
			return nil, fmt.Errorf("decode register %s: %w", definition.Key, err)
		}
		info[definition.Key] = value
	}
	return info, nil
}

func decodeRawValue(registers []uint16, definition registerDef) (any, error) {
	value, err := decodeRegisterValue(registers, definition)
	if err != nil {
		return nil, err
	}
	return value.value(), nil
}

func decodeRegisterValue(registers []uint16, definition registerDef) (rawRegisterValue, error) {
	if definition.Start < 0 || definition.Length < 1 ||
		definition.Start+definition.Length > len(registers) {
		return rawRegisterValue{}, fmt.Errorf("register definition is outside read buffer")
	}
	words := registers[definition.Start : definition.Start+definition.Length]
	switch definition.Type {
	case registerChar:
		text := decodeRegisterString(words)
		if !utf8.ValidString(text) {
			text = strings.ToValidUTF8(text, "\uFFFD")
		}
		return rawRegisterValue{text: text, isText: true}, nil
	case registerUint, registerInt:
		if definition.Length > 4 {
			return rawRegisterValue{}, fmt.Errorf("numeric register is too wide")
		}
		var unsigned uint64
		for _, word := range words {
			unsigned = unsigned<<16 | uint64(word)
		}
		if definition.Type == registerInt {
			bits := definition.Length * 16
			if unsigned&(uint64(1)<<(bits-1)) != 0 {
				return rawRegisterValue{integer: int64(unsigned - (uint64(1) << bits))}, nil
			}
		}
		return rawRegisterValue{integer: int64(unsigned)}, nil
	default:
		return rawRegisterValue{}, fmt.Errorf("invalid register type")
	}
}

func decodeRegisterString(words []uint16) string {
	length := len(words) * 2
	for length > 0 {
		word := words[(length-1)/2]
		var character byte
		if length%2 == 0 {
			character = byte(word)
		} else {
			character = byte(word >> 8)
		}
		if character != 0 {
			break
		}
		length--
	}

	var builder strings.Builder
	builder.Grow(length)
	for index := 0; index < length; index++ {
		word := words[index/2]
		if index%2 == 0 {
			builder.WriteByte(byte(word >> 8))
		} else {
			builder.WriteByte(byte(word))
		}
	}
	return builder.String()
}

func (i *Inverter) WriteConfig(ctx context.Context, key string, value any) (any, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	definition, ok := holdingByKey[key]
	if !ok {
		return nil, &configCommandError{Err: fmt.Errorf("invalid key %q", key)}
	}
	if definition.Write == nil {
		return nil, &configCommandError{Err: fmt.Errorf("register %s is not writeable", key)}
	}
	processed, err := definition.Write(value)
	if err != nil {
		return nil, &configCommandError{Err: fmt.Errorf("invalid value for %s: %w", key, err)}
	}
	values, err := encodeRawValue(processed, definition)
	if err != nil {
		return nil, &configCommandError{Err: fmt.Errorf("invalid value for %s: %w", key, err)}
	}
	if err := i.client.WriteRegisters(ctx, definition.Start, func() ([]uint16, error) {
		return values, nil
	}); err != nil {
		return nil, err
	}
	if i.pendingReadbacks == nil {
		i.pendingReadbacks = make(map[string]pendingReadback)
	}
	i.pendingReadbacks[key] = pendingReadback{
		start:          definition.Start,
		expectedValues: append([]uint16(nil), values...),
	}
	raw, err := decodeRegisterValue(values, registerDef{
		Start: 0, Length: definition.Length, Type: definition.Type,
	})
	if err != nil {
		return nil, fmt.Errorf("decode written register %s: %w", key, err)
	}
	acknowledgedValue, err := definition.Read(raw)
	if err != nil {
		return nil, fmt.Errorf("decode written value %s: %w", key, err)
	}
	slog.Info("config register written", "key", key, "value", acknowledgedValue)
	return acknowledgedValue, nil
}

func encodeRawValue(value any, definition registerDef) ([]uint16, error) {
	if definition.Type == registerChar {
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("text register requires a string")
		}
		if len(text) > definition.Length*2 {
			return nil, fmt.Errorf("value is longer than %d bytes", definition.Length*2)
		}
		values := make([]uint16, definition.Length)
		for index := 0; index < len(text); index++ {
			if index%2 == 0 {
				values[index/2] = uint16(text[index]) << 8
			} else {
				values[index/2] |= uint16(text[index])
			}
		}
		return values, nil
	}

	number, err := asInt64(value)
	if err != nil {
		return nil, err
	}
	bits := definition.Length * 16
	if definition.Type == registerUint {
		if number < 0 || (bits < 64 && uint64(number) >= uint64(1)<<bits) {
			return nil, fmt.Errorf("value does not fit unsigned %d-bit register", bits)
		}
	} else {
		minimum := -int64(1) << (bits - 1)
		maximum := int64(1)<<(bits-1) - 1
		if number < minimum || number > maximum {
			return nil, fmt.Errorf("value does not fit signed %d-bit register", bits)
		}
	}

	values := make([]uint16, definition.Length)
	unsigned := uint64(number)
	for index := definition.Length - 1; index >= 0; index-- {
		values[index] = uint16(unsigned)
		unsigned >>= 16
	}
	return values, nil
}

func (i *Inverter) reconcilePendingReadback(registers []uint16) {
	for key, pending := range i.pendingReadbacks {
		if pending.start+len(pending.expectedValues) > len(registers) {
			slog.Warn("config write could not be verified by read-back", "key", key)
			continue
		}
		current := registers[pending.start : pending.start+len(pending.expectedValues)]
		if equalRegisters(current, pending.expectedValues) {
			slog.Info("config write verified by read-back", "key", key)
		} else {
			slog.Warn("config write not visible in read-back",
				"key", key, "expected", pending.expectedValues, "read", current)
		}
	}
}

func equalRegisters(left, right []uint16) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func finiteScalar(raw string) (any, error) {
	parsed, err := strconvParseFloat(raw)
	if err != nil {
		return raw, nil
	}
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return nil, fmt.Errorf("invalid value")
	}
	rounded := math.Round(parsed)
	if math.Abs(parsed-rounded) < 1e-3 {
		return int64(rounded), nil
	}
	return parsed, nil
}

var strconvParseFloat = func(raw string) (float64, error) {
	return strconv.ParseFloat(raw, 64)
}
