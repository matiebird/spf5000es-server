package main

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"testing"
	"unicode/utf8"
)

func TestDecodeNumericRegisters(t *testing.T) {
	tests := []struct {
		name       string
		words      []uint16
		registerTy registerType
		want       int64
	}{
		{name: "unsigned 16-bit", words: []uint16{0xffff}, registerTy: registerUint, want: 65535},
		{name: "unsigned 32-bit", words: []uint16{0x1234, 0x5678}, registerTy: registerUint, want: 0x12345678},
		{name: "signed 16-bit negative", words: []uint16{0xffff}, registerTy: registerInt, want: -1},
		{name: "signed 32-bit minimum", words: []uint16{0x8000, 0}, registerTy: registerInt, want: math.MinInt32},
		{name: "signed 64-bit minimum", words: []uint16{0x8000, 0, 0, 0}, registerTy: registerInt, want: math.MinInt64},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := decodeRawValue(test.words, registerDef{Length: len(test.words), Type: test.registerTy})
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("decoded = %v, want %d", got, test.want)
			}
		})
	}
}

func TestReadRegisterWindowValidatesCount(t *testing.T) {
	for _, start := range []int{0, 5} {
		t.Run(fmt.Sprintf("start_%d", start), func(t *testing.T) {
			window := registerWindow{Start: start, Count: 2}
			reader := func(context.Context, int, int) ([]uint16, error) {
				return []uint16{42}, nil
			}
			if _, err := readRegisterWindow(context.Background(), reader, window); err == nil {
				t.Fatal("short register read was accepted")
			}
		})
	}
}

func TestReadRegisterWindowsValidatesCount(t *testing.T) {
	windows := []registerWindow{{Start: 0, Count: 2}, {Start: 2, Count: 2}}
	reader := func(_ context.Context, start, count int) ([]uint16, error) {
		if start == 2 {
			return make([]uint16, count+1), nil
		}
		return make([]uint16, count), nil
	}
	if _, err := readRegisterWindows(context.Background(), reader, windows); err == nil {
		t.Fatal("long register read was accepted")
	}
}

func TestDecodeRegisterTextSanitizesAndTrims(t *testing.T) {
	tests := []struct {
		name  string
		words []uint16
		want  string
	}{
		{name: "trailing nulls", words: []uint16{0x7631, 0x2e32, 0}, want: "v1.2"},
		{name: "interior null", words: []uint16{0x4100, 0x4200}, want: "A\x00B"},
		{name: "all null", words: []uint16{0, 0}, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := decodeRawValue(test.words, registerDef{Length: len(test.words), Type: registerChar})
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("decoded = %q, want %q", got, test.want)
			}
		})
	}

	got, err := decodeRawValue([]uint16{0xfffe, 0x4100}, registerDef{Length: 2, Type: registerChar})
	if err != nil {
		t.Fatal(err)
	}
	text := got.(string)
	if !utf8.ValidString(text) || text[len(text)-1] != 'A' {
		t.Fatalf("invalid UTF-8 was not sanitized: %q", text)
	}
}

func TestDecodeRegisterTableTransformsTypedValues(t *testing.T) {
	definitions := []registerDef{
		{Key: "status", Start: 0, Length: 1, Type: registerUint, Read: systemStatus},
		{Key: "scaled", Start: 1, Length: 1, Type: registerUint, Read: scaled(10)},
		{Key: "boolean", Start: 2, Length: 1, Type: registerUint, Read: asBool},
		{Key: "signed", Start: 3, Length: 1, Type: registerInt, Read: identity},
	}
	got, err := decodeRegisterTable([]uint16{1, 123, 1, 0xffff}, definitions)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"status": "PV&Grid Supporting Loads", "scaled": 12.3,
		"boolean": true, "signed": int64(-1),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded = %#v, want %#v", got, want)
	}
}

func TestRegisterTextRoundTrip(t *testing.T) {
	definition := registerDef{Length: 3, Type: registerChar}
	encoded, err := encodeRawValue("v1.2", definition)
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint16{0x7631, 0x2e32, 0}; !reflect.DeepEqual(encoded, want) {
		t.Fatalf("encoded = %#v, want %#v", encoded, want)
	}

	definition.Start = 0
	decoded, err := decodeRawValue(encoded, definition)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != "v1.2" {
		t.Fatalf("decoded = %q, want %q", decoded, "v1.2")
	}
}

func TestEncodeNumericRegisters(t *testing.T) {
	definition := registerDef{Length: 2, Type: registerUint}
	encoded, err := encodeRawValue(int64(0x12345678), definition)
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint16{0x1234, 0x5678}; !reflect.DeepEqual(encoded, want) {
		t.Fatalf("encoded = %#v, want %#v", encoded, want)
	}
}

func TestRegisterWriteTransformsValidateCommandValues(t *testing.T) {
	tests := []struct {
		name      string
		transform writeValueFunc
		value     any
		want      any
		wantError bool
	}{
		{name: "boolean text", transform: boolWrite, value: "on", want: int64(1)},
		{name: "scaled decimal", transform: scaleWrite(10), value: "52.4", want: int64(524)},
		{name: "enum option", transform: enumWrite(reverseEnum(outputConfig)), value: "SUB", want: int64(3)},
		{name: "invalid boolean", transform: boolWrite, value: "sometimes", wantError: true},
		{name: "non-finite number", transform: scaleWrite(10), value: "NaN", wantError: true},
		{name: "unknown enum option", transform: enumWrite(reverseEnum(outputConfig)), value: "invalid", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.transform(test.value)
			if test.wantError {
				if err == nil {
					t.Fatalf("transform(%v) succeeded with %v", test.value, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("transform(%v) = %#v, want %#v", test.value, got, test.want)
			}
		})
	}
}

func BenchmarkDecodeRegisterText(b *testing.B) {
	registers := []uint16{0x7631, 0x2e32, 0}
	definition := registerDef{Length: len(registers), Type: registerChar, Read: identity}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := decodeRawValue(registers, definition); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeStatusRegisters(b *testing.B) {
	registers := make([]uint16, inputRegisterWindow.Start+inputRegisterWindow.Count)
	registers[0] = 1
	for index := 1; index < len(registers); index++ {
		registers[index] = uint16(1000 + index)
	}
	b.ReportAllocs()
	for b.Loop() {
		status, err := decodeRegisterTable(registers, inputRegisters)
		if err != nil {
			b.Fatal(err)
		}
		if len(status) != len(inputRegisters) {
			b.Fatalf("decoded %d registers, want %d", len(status), len(inputRegisters))
		}
	}
}

func BenchmarkEncodeNumericRegisters(b *testing.B) {
	definition := registerDef{Length: 2, Type: registerUint}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := encodeRawValue(int64(0x12345678), definition); err != nil {
			b.Fatal(err)
		}
	}
}

var benchmarkMQTTPayload []byte

func BenchmarkMQTTPayload(b *testing.B) {
	values := []any{"online", int64(1234), 23.4, true}
	b.Run("direct bytes", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			for _, value := range values {
				benchmarkMQTTPayload = mqttValue(value)
			}
		}
	})
	b.Run("string then Paho copy", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			for _, value := range values {
				benchmarkMQTTPayload = []byte(benchmarkMQTTStringValue(value))
			}
		}
	})
}

func benchmarkMQTTStringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	}
	return fmt.Sprint(value)
}
