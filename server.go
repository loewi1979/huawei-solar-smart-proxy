package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/goburrow/modbus"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Server is a Modbus TCP server that serves register values from cache
// and forwards writes to the inverter.
type Server struct {
	cfg            *Config
	cache          *RegisterCache
	inverterClient atomic.Pointer[InverterClient]
}

func NewServer(cfg *Config, cache *RegisterCache) *Server {
	return &Server{
		cfg:   cfg,
		cache: cache,
	}
}

// SetInverterClient attaches the inverter client once it's connected.
// Before this is called, cache misses and writes return a device failure exception.
func (s *Server) SetInverterClient(client *InverterClient) {
	s.inverterClient.Store(client)
}

// ListenAndServe starts accepting Modbus TCP connections.
func (s *Server) ListenAndServe(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Server.Listen, err)
	}

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	slog.Info("modbus TCP server listening", "address", s.cfg.Server.Listen)
	var connections sync.WaitGroup
	defer connections.Wait()
	slots := make(chan struct{}, 64)

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Error("accept error", "error", err)
			continue
		}

		slog.Info("client connected", "remote", conn.RemoteAddr())
		select {
		case slots <- struct{}{}:
			connections.Add(1)
			go func() { defer connections.Done(); defer func() { <-slots }(); s.handleConnection(ctx, conn) }()
		default:
			conn.Close()
		}
	}
}

func (s *Server) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	defer slog.Info("client disconnected", "remote", conn.RemoteAddr())
	var authGeneration uint64

	for {
		if ctx.Err() != nil {
			return
		}

		// Read MBAP header: 7 bytes
		// [0:2] Transaction ID
		// [2:4] Protocol ID (must be 0)
		// [4:6] Length (remaining bytes including unit ID)
		// [6]   Unit ID
		header := make([]byte, 7)
		conn.SetDeadline(time.Now().Add(30 * time.Second))
		if _, err := io.ReadFull(conn, header); err != nil {
			if err != io.EOF {
				slog.Debug("read header error", "error", err)
			}
			return
		}

		txID := binary.BigEndian.Uint16(header[0:2])
		protocolID := binary.BigEndian.Uint16(header[2:4])
		length := binary.BigEndian.Uint16(header[4:6])
		unitID := header[6]

		if protocolID != 0 {
			slog.Warn("invalid protocol ID, closing connection", "protocol_id", protocolID)
			return
		}

		if length < 2 || length > 254 {
			slog.Warn("invalid MBAP length, closing connection", "length", length)
			return
		}

		// Read PDU: length-1 bytes (length includes the unit ID we already read)
		pdu := make([]byte, length-1)
		if _, err := io.ReadFull(conn, pdu); err != nil {
			slog.Debug("read PDU error", "error", err)
			return
		}

		fc := pdu[0]
		// A reconnect destroys the Huawei login session. Closing this client
		// forces HA to reconnect/re-authenticate instead of using stale privileges.
		if authGeneration != 0 {
			client := s.inverterClient.Load()
			if client == nil || client.ConnectionGeneration() != authGeneration {
				return
			}
		}
		allowed := false
		for _, u := range s.cfg.Inverter.UnitIDs {
			if u == unitID {
				allowed = true
			}
		}
		if !allowed {
			sendException(conn, txID, unitID, fc, 0x0b)
			continue
		}
		if (fc == 0x06 || fc == 0x10) && s.cfg.ReadOnly {
			sendException(conn, txID, unitID, fc, 0x01)
			continue
		}

		switch fc {
		case 0x2b, 0x41:
			response, generation := s.handleHuaweiPDU(unitID, pdu)
			if fc == 0x41 && len(response) >= 2 && response[0] == 0x41 && (pdu[1] == 0x24 || pdu[1] == 0x25) {
				authGeneration = generation
			}
			sendPDUResponse(conn, txID, unitID, response)
		case 0x03, 0x04: // Read Holding Registers / Read Input Registers
			s.handleReadRegisters(conn, txID, unitID, fc, pdu)
		case 0x06: // Write Single Register
			s.handleWriteSingleRegister(conn, txID, unitID, pdu)
		case 0x10: // Write Multiple Registers
			s.handleWriteMultipleRegisters(conn, txID, unitID, pdu)
		default:
			slog.Warn("unsupported function code", "fc", fc)
			sendException(conn, txID, unitID, fc, 0x01) // Illegal Function
		}
	}
}

func (s *Server) handleHuaweiPDU(unit byte, pdu []byte) ([]byte, uint64) {
	fc := pdu[0]
	exception := func(code byte) ([]byte, uint64) { return []byte{fc | 0x80, code}, 0 }
	if fc == 0x41 && s.cfg.ReadOnly {
		return exception(0x01)
	}
	if fc == 0x2b {
		// Read Device Identification (MEI 0x0e) is read-only, not a generic
		// encapsulated-interface passthrough.
		if len(pdu) != 4 || pdu[1] != 0x0e || pdu[2] < 1 || pdu[2] > 4 {
			return exception(0x03)
		}
	} else {
		if len(pdu) < 2 {
			return exception(0x03)
		}
		switch pdu[1] {
		case 0x24:
			if len(pdu) != 4 || pdu[2] != 1 || pdu[3] != 0 {
				return exception(0x03)
			}
		case 0x25:
			if len(pdu) < 54 || int(pdu[2]) != len(pdu)-3 {
				return exception(0x03)
			}
			usernameLength := int(pdu[19])
			digestOffset := 20 + usernameLength
			if usernameLength == 0 || digestOffset >= len(pdu) || pdu[digestOffset] != 32 || len(pdu) != digestOffset+33 {
				return exception(0x03)
			}
		default:
			return exception(0x01)
		}
	}
	client := s.inverterClient.Load()
	if client == nil {
		return exception(0x04)
	}
	response, generation, err := client.ForwardPDU(unit, pdu)
	if err != nil {
		slog.Warn("Huawei protocol forwarding failed", "function", fc, "unit", unit, "error", err)
		return exception(0x04)
	}
	return response, generation
}

func sendPDUResponse(conn net.Conn, txID uint16, unit byte, pdu []byte) {
	response := make([]byte, 7+len(pdu))
	binary.BigEndian.PutUint16(response, txID)
	binary.BigEndian.PutUint16(response[4:], uint16(1+len(pdu)))
	response[6] = unit
	copy(response[7:], pdu)
	conn.Write(response)
}

func (s *Server) handleReadRegisters(conn net.Conn, txID uint16, unitID byte, fc byte, pdu []byte) {
	if len(pdu) != 5 {
		sendException(conn, txID, unitID, fc, 0x03)
		return
	}

	address := binary.BigEndian.Uint16(pdu[1:3])
	count := binary.BigEndian.Uint16(pdu[3:5])

	if count == 0 || count > 125 || uint32(address)+uint32(count) > 65536 {
		sendException(conn, txID, unitID, fc, 0x03) // Illegal Data Value
		return
	}

	// Try cache first
	values, _, freshErr := s.cache.FreshConfigured(unitID, address, count, s.cfg, time.Now())
	if freshErr == nil {
		data := make([]byte, len(values)*2)
		for i, v := range values {
			binary.BigEndian.PutUint16(data[i*2:], v)
		}
		slog.Debug("cache hit", "unit_id", unitID, "address", address, "count", count)
		sendReadResponse(conn, txID, unitID, fc, data)
		return
	}
	if s.cfg.KnownRange(address, count) {
		sendException(conn, txID, unitID, fc, 0x0b)
		return
	}

	// Cache miss — forward if enabled and inverter client is available
	client := s.inverterClient.Load()
	if s.cfg.ForwardUnknownReads && client != nil {
		slog.Info("cache miss, forwarding to inverter", "unit_id", unitID, "address", address, "count", count)
		result, err := client.ReadRegisters(unitID, address, count)
		if err != nil {
			slog.Warn("forward read failed", "unit_id", unitID, "address", address, "count", count, "error", err)
			sendException(conn, txID, unitID, fc, upstreamException(err))
			return
		}
		// Cache the forwarded result
		s.cache.SetFromBytes(unitID, address, result)
		sendReadResponse(conn, txID, unitID, fc, result)
		return
	}

	slog.Debug("cache miss, forwarding disabled", "address", address, "count", count)
	sendException(conn, txID, unitID, fc, 0x02) // Illegal Data Address
}

func (s *Server) handleWriteSingleRegister(conn net.Conn, txID uint16, unitID byte, pdu []byte) {
	if s.cfg.ReadOnly {
		sendException(conn, txID, unitID, 0x06, 0x01)
		return
	}
	if len(pdu) != 5 {
		sendException(conn, txID, unitID, 0x06, 0x03)
		return
	}

	address := binary.BigEndian.Uint16(pdu[1:3])
	value := binary.BigEndian.Uint16(pdu[3:5])

	slog.Info("forwarding write single register", "unit_id", unitID, "address", address, "value", value)

	client := s.inverterClient.Load()
	if client == nil {
		slog.Warn("write single register: inverter not connected", "unit_id", unitID, "address", address)
		sendException(conn, txID, unitID, 0x06, 0x04)
		return
	}

	_, err := client.WriteSingleRegister(unitID, address, value)
	if err != nil {
		slog.Warn("write single register failed", "unit_id", unitID, "address", address, "error", err)
		sendException(conn, txID, unitID, 0x06, upstreamException(err))
		return
	}

	// Update cache
	s.cache.Set(unitID, address, []uint16{value})

	// Response echoes back address + value
	sendWriteSingleResponse(conn, txID, unitID, address, value)
}

func (s *Server) handleWriteMultipleRegisters(conn net.Conn, txID uint16, unitID byte, pdu []byte) {
	if s.cfg.ReadOnly {
		sendException(conn, txID, unitID, 0x10, 0x01)
		return
	}
	if len(pdu) < 6 {
		sendException(conn, txID, unitID, 0x10, 0x03)
		return
	}

	address := binary.BigEndian.Uint16(pdu[1:3])
	count := binary.BigEndian.Uint16(pdu[3:5])
	byteCount := pdu[5]

	if count == 0 || count > 123 || uint32(address)+uint32(count) > 65536 || int(byteCount) != int(count)*2 || len(pdu) != 6+int(byteCount) {
		sendException(conn, txID, unitID, 0x10, 0x03)
		return
	}

	data := pdu[6 : 6+byteCount]

	slog.Info("forwarding write multiple registers", "unit_id", unitID, "address", address, "count", count)

	client := s.inverterClient.Load()
	if client == nil {
		slog.Warn("write multiple registers: inverter not connected", "unit_id", unitID, "address", address)
		sendException(conn, txID, unitID, 0x10, 0x04)
		return
	}

	_, err := client.WriteMultipleRegisters(unitID, address, count, data)
	if err != nil {
		slog.Warn("write multiple registers failed", "unit_id", unitID, "address", address, "error", err)
		sendException(conn, txID, unitID, 0x10, upstreamException(err))
		return
	}

	// Update cache
	s.cache.SetFromBytes(unitID, address, data)

	sendWriteMultipleResponse(conn, txID, unitID, address, count)
}

// --- Modbus TCP response helpers ---

// HA uses Huawei 0x80 (permission denied) / 0x02 to decide whether login is
// needed. Replacing every inverter exception with 0x04 breaks that decision.
func upstreamException(err error) byte {
	var mb *modbus.ModbusError
	if errors.As(err, &mb) && mb.ExceptionCode != 0 {
		return mb.ExceptionCode
	}
	return 0x04
}

func sendReadResponse(conn net.Conn, txID uint16, unitID byte, fc byte, data []byte) {
	byteCount := byte(len(data))
	mbapLen := uint16(3 + len(data)) // unitID + fc + byteCount + data

	resp := make([]byte, 0, 9+len(data))
	resp = binary.BigEndian.AppendUint16(resp, txID)
	resp = binary.BigEndian.AppendUint16(resp, 0) // protocol ID
	resp = binary.BigEndian.AppendUint16(resp, mbapLen)
	resp = append(resp, unitID, fc, byteCount)
	resp = append(resp, data...)

	conn.Write(resp)
}

func sendException(conn net.Conn, txID uint16, unitID byte, fc byte, exCode byte) {
	resp := make([]byte, 0, 9)
	resp = binary.BigEndian.AppendUint16(resp, txID)
	resp = binary.BigEndian.AppendUint16(resp, 0) // protocol ID
	resp = binary.BigEndian.AppendUint16(resp, 3) // length: unitID + errorFC + exCode
	resp = append(resp, unitID, fc|0x80, exCode)

	conn.Write(resp)
}

func sendWriteSingleResponse(conn net.Conn, txID uint16, unitID byte, address, value uint16) {
	resp := make([]byte, 0, 12)
	resp = binary.BigEndian.AppendUint16(resp, txID)
	resp = binary.BigEndian.AppendUint16(resp, 0) // protocol ID
	resp = binary.BigEndian.AppendUint16(resp, 6) // length: unitID + fc + addr(2) + value(2)
	resp = append(resp, unitID, 0x06)
	resp = binary.BigEndian.AppendUint16(resp, address)
	resp = binary.BigEndian.AppendUint16(resp, value)

	conn.Write(resp)
}

func sendWriteMultipleResponse(conn net.Conn, txID uint16, unitID byte, address, count uint16) {
	resp := make([]byte, 0, 12)
	resp = binary.BigEndian.AppendUint16(resp, txID)
	resp = binary.BigEndian.AppendUint16(resp, 0) // protocol ID
	resp = binary.BigEndian.AppendUint16(resp, 6) // length: unitID + fc + addr(2) + count(2)
	resp = append(resp, unitID, 0x10)
	resp = binary.BigEndian.AppendUint16(resp, address)
	resp = binary.BigEndian.AppendUint16(resp, count)

	conn.Write(resp)
}
