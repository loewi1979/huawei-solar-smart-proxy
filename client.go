package main

// All upstream Modbus traffic shares one TCP connection and request pacing.
// The connection generation tracks the lifetime of Huawei login sessions.

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goburrow/modbus"
)

// InverterClient serializes access to the TCP handler, selected unit ID,
// connection state and timing under mu. The generation can be read without
// taking mu. The pauses and write policy are fixed at construction.
type InverterClient struct {
	mu               sync.Mutex
	client           modbus.Client
	handler          *modbus.TCPClientHandler
	pauseMs          int
	lastRead         time.Time
	connected        bool
	waitAfterConnect time.Duration
	generation       atomic.Uint64
	readOnly         bool
}

// NewInverterClient configures the shared handler. The first operation opens
// the connection, so startup does not depend on inverter availability.
func NewInverterClient(cfg *Config) (*InverterClient, error) {
	handler := modbus.NewTCPClientHandler(cfg.InverterAddr())
	handler.Timeout = cfg.InverterTimeout()
	handler.IdleTimeout = 0 // keep connection alive

	client := modbus.NewClient(handler)

	return &InverterClient{
		client:           client,
		handler:          handler,
		pauseMs:          cfg.Polling.ReadPauseMs,
		waitAfterConnect: time.Duration(cfg.Inverter.WaitAfterConnectMs) * time.Millisecond,
		readOnly:         cfg.ReadOnly,
	}, nil
}

// Close closes the current connection and invalidates its generation.
func (ic *InverterClient) Close() {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	ic.handler.Close()
	if ic.connected {
		ic.generation.Add(1)
	}
	ic.connected = false
}

// connectLocked requires mu. The SDongle needs a quiet period after each
// successful TCP connect to receive the first request reliably.
func (ic *InverterClient) connectLocked() error {
	if ic.connected {
		return nil
	}
	if err := ic.handler.Connect(); err != nil {
		return err
	}
	time.Sleep(ic.waitAfterConnect)
	ic.connected = true
	ic.generation.Add(1)
	return nil
}

// finishLocked requires mu and records when an attempt ends. A Modbus
// exception is a valid response, so the session stays open. Other errors
// clear the connection and invalidate its generation.
func (ic *InverterClient) finishLocked(err error) {
	ic.lastRead = time.Now()
	var responseError *modbus.ModbusError
	if err != nil && !errors.As(err, &responseError) {
		ic.handler.Close()
		if ic.connected {
			ic.generation.Add(1)
		}
		ic.connected = false
	}
}

// enforcePause requires mu. It waits out the request gap from the end of
// the previous attempt, including failed attempts.
func (ic *InverterClient) enforcePause() {
	if ic.lastRead.IsZero() {
		return
	}
	elapsed := time.Since(ic.lastRead)
	gap := time.Duration(ic.pauseMs)*time.Millisecond - elapsed
	if gap > 0 {
		time.Sleep(gap)
	}
}

// ReadRegisters reads holding registers for one unit through the shared,
// paced upstream connection.
func (ic *InverterClient) ReadRegisters(unitID byte, address, count uint16) ([]byte, error) {
	ic.mu.Lock()
	defer ic.mu.Unlock()

	ic.handler.SlaveId = unitID
	ic.enforcePause()
	if err := ic.connectLocked(); err != nil {
		ic.finishLocked(err)
		return nil, err
	}
	started := time.Now()
	result, err := ic.client.ReadHoldingRegisters(address, count)
	slog.Debug("inverter read completed", "unit", unitID, "address", address, "count", count,
		"latency_ms", float64(time.Since(started).Microseconds())/1000, "success", err == nil)
	ic.finishLocked(err)
	return result, err
}

// WriteSingleRegister writes one holding register when write mode is enabled.
// It returns upstream Modbus exceptions for the server to relay.
func (ic *InverterClient) WriteSingleRegister(unitID byte, address, value uint16) ([]byte, error) {
	if ic.readOnly {
		return nil, fmt.Errorf("inverter writes disabled by read_only")
	}
	ic.mu.Lock()
	defer ic.mu.Unlock()

	ic.handler.SlaveId = unitID
	ic.enforcePause()
	if err := ic.connectLocked(); err != nil {
		ic.finishLocked(err)
		return nil, err
	}
	result, err := ic.client.WriteSingleRegister(address, value)
	ic.finishLocked(err)
	return result, err
}

// WriteMultipleRegisters writes a contiguous range of holding registers when
// write mode is enabled. The payload is in Modbus byte order.
func (ic *InverterClient) WriteMultipleRegisters(unitID byte, address, count uint16, data []byte) ([]byte, error) {
	if ic.readOnly {
		return nil, fmt.Errorf("inverter writes disabled by read_only")
	}
	ic.mu.Lock()
	defer ic.mu.Unlock()

	ic.handler.SlaveId = unitID
	ic.enforcePause()
	if err := ic.connectLocked(); err != nil {
		ic.finishLocked(err)
		return nil, err
	}
	result, err := ic.client.WriteMultipleRegisters(address, count, data)
	ic.finishLocked(err)
	return result, err
}

// Reconnect clears the current upstream session. The next operation opens
// a connection and observes the request gap and connect quiet period.
func (ic *InverterClient) Reconnect() {
	ic.mu.Lock()
	defer ic.mu.Unlock()

	ic.handler.Close()
	if ic.connected {
		ic.generation.Add(1)
	}
	ic.connected = false
}

// ConnectionGeneration identifies the current upstream connection lifetime.
// Changes on connection open or invalidation let the server expire downstream
// Huawei login state when the upstream connection changes.
func (ic *InverterClient) ConnectionGeneration() uint64 { return ic.generation.Load() }

// ForwardPDU relays a raw device-identification or Huawei login exchange and
// returns the generation that carried it, captured under mu. The server must
// validate the function, subfunction and read-only policy before calling.
// The client also enforces the read-only restriction on private functions.
func (ic *InverterClient) ForwardPDU(unit byte, pdu []byte) (response []byte, generation uint64, err error) {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	defer func() { ic.finishLocked(err) }()
	if len(pdu) < 2 || len(pdu) > 253 {
		return nil, 0, fmt.Errorf("invalid PDU length")
	}
	if ic.readOnly && pdu[0] != 0x2b {
		return nil, 0, fmt.Errorf("private Huawei functions disabled by read_only")
	}
	ic.handler.SlaveId = unit
	ic.enforcePause()
	if err = ic.connectLocked(); err != nil {
		return nil, 0, err
	}
	request, err := ic.handler.Encode(&modbus.ProtocolDataUnit{FunctionCode: pdu[0], Data: pdu[1:]})
	if err != nil {
		return nil, 0, err
	}
	adu, err := ic.handler.Send(request)
	if err != nil {
		return nil, 0, err
	}
	if len(adu) < 8 {
		return nil, 0, fmt.Errorf("short upstream PDU")
	}
	if err = ic.handler.Verify(request, adu); err != nil {
		return nil, 0, err
	}
	decoded, err := ic.handler.Decode(adu)
	if err != nil {
		return nil, 0, err
	}
	// Relay a well-formed exception unchanged so HA can handle the inverter's
	// response. A successful response must match the requested operation.
	response = append([]byte{decoded.FunctionCode}, decoded.Data...)
	if response[0] == pdu[0]|0x80 {
		if len(response) != 2 {
			return nil, 0, fmt.Errorf("invalid upstream exception")
		}
	} else if response[0] != pdu[0] || len(response) < 2 || response[1] != pdu[1] {
		return nil, 0, fmt.Errorf("unexpected upstream function or subfunction")
	}
	return response, ic.generation.Load(), nil
}

// IsDeviceBusy returns true if the error is a Modbus exception 0x06
// (server device busy).
func IsDeviceBusy(err error) bool {
	var mbErr *modbus.ModbusError
	if errors.As(err, &mbErr) {
		return mbErr.ExceptionCode == modbus.ExceptionCodeServerDeviceBusy
	}
	return false
}

// IsTransactionMismatch returns true if the error indicates a Modbus TCP
// transaction ID mismatch. The goburrow library reports this as a plain
// fmt.Errorf, so we match on the error string.
func IsTransactionMismatch(err error) bool {
	return err != nil && strings.Contains(err.Error(), "transaction id")
}

// IsTimeout returns true if the error is a network timeout (i/o timeout).
func IsTimeout(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return errors.Is(err, os.ErrDeadlineExceeded)
}

// IsConnectionError returns true if the error indicates a broken TCP
// connection (broken pipe, connection reset, EOF).
func IsConnectionError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "EOF")
}
