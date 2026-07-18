package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/grid-x/modbus"
)

const (
	maxHoldingReadRegisters = 45
	registerWriteInterval   = 850 * time.Millisecond
)

type fatalModbusError struct {
	Err error
}

func (e *fatalModbusError) Error() string {
	return fmt.Sprintf("Modbus operation failed: %v", e.Err)
}

func (e *fatalModbusError) Unwrap() error { return e.Err }

func isFatalModbus(err error) bool {
	var fatal *fatalModbusError
	return errors.As(err, &fatal)
}

type slogModbusLogger struct{}

func (slogModbusLogger) Printf(format string, values ...any) {
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	slog.Debug(fmt.Sprintf(format, values...))
}

type ModbusClient struct {
	mu                sync.Mutex
	handler           *modbus.RTUClientHandler
	client            modbus.Client
	timeout           time.Duration
	lastRegisterWrite time.Time
}

func NewModbusClient(config ModbusConfig) *ModbusClient {
	handler := modbus.NewRTUClientHandler(config.Port)
	handler.BaudRate = 9600
	handler.DataBits = 8
	handler.Parity = "N"
	handler.StopBits = 1
	handler.SlaveID = 1
	handler.Timeout = config.Timeout
	handler.IdleTimeout = 0
	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		handler.Logger = slogModbusLogger{}
	}
	return &ModbusClient{
		handler: handler,
		client:  modbus.NewClient(handler),
		timeout: config.Timeout,
	}
}

func (c *ModbusClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.execute(ctx, func(requestCtx context.Context) error {
		return c.handler.Connect(requestCtx)
	})
}

func (c *ModbusClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fatalModbusErrorFor(c.handler.Close())
}

func (c *ModbusClient) ReadInputRegisters(ctx context.Context, start, count int) ([]uint16, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var result []byte
	err := c.execute(ctx, func(requestCtx context.Context) error {
		var operationErr error
		result, operationErr = c.client.ReadInputRegisters(requestCtx, uint16(start), uint16(count))
		return operationErr
	})
	if err != nil {
		return nil, err
	}
	return bytesToRegisters(result), nil
}

func (c *ModbusClient) ReadHoldingRegisters(ctx context.Context, start, count int) ([]uint16, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var result []byte
	err := c.execute(ctx, func(requestCtx context.Context) error {
		var operationErr error
		result, operationErr = c.client.ReadHoldingRegisters(requestCtx, uint16(start), uint16(count))
		return operationErr
	})
	if err != nil {
		return nil, err
	}
	return bytesToRegisters(result), nil
}

func (c *ModbusClient) WriteRegisters(
	ctx context.Context,
	start int,
	values func() ([]uint16, error),
) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.lastRegisterWrite.IsZero() {
		wait := registerWriteInterval - time.Since(c.lastRegisterWrite)
		if err := sleepContext(ctx, wait); err != nil {
			return err
		}
	}
	registerValues, err := values()
	if err != nil {
		return err
	}

	err = c.execute(ctx, func(requestCtx context.Context) error {
		if len(registerValues) == 1 {
			_, err := c.client.WriteSingleRegister(requestCtx, uint16(start), registerValues[0])
			return err
		}

		data := registersToBytes(registerValues)
		_, err := c.client.WriteMultipleRegisters(
			requestCtx, uint16(start), uint16(len(registerValues)), data,
		)
		return err
	})
	c.lastRegisterWrite = time.Now()
	if err != nil {
		return err
	}
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		slog.DebugContext(ctx, "Modbus registers written", "start", start, "values", registerValues)
	}
	return nil
}

func (c *ModbusClient) execute(ctx context.Context, operation func(context.Context) error) error {
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	err := operation(requestCtx)
	cancel()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fatalModbusErrorFor(err)
}

func fatalModbusErrorFor(err error) error {
	if err == nil || isFatalModbus(err) {
		return err
	}
	return &fatalModbusError{Err: err}
}

func registersToBytes(registers []uint16) []byte {
	data := make([]byte, len(registers)*2)
	for index, value := range registers {
		binary.BigEndian.PutUint16(data[index*2:], value)
	}
	return data
}

func bytesToRegisters(data []byte) []uint16 {
	registers := make([]uint16, len(data)/2)
	for index := range registers {
		registers[index] = binary.BigEndian.Uint16(data[index*2:])
	}
	return registers
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
