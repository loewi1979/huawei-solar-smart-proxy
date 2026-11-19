package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// TestReadOnlyConfiguration covers the default policy, both spellings and
// ambiguous declarations, including one introduced by a YAML merge.
func TestReadOnlyConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		readOnly   bool
	}{
		{"default", "", true}, {"protected", "read_only: true\n", true},
		{"write enabled", "read_only: false\n", false},
		{"legacy protected", "allow_writes: false\n", true},
		{"legacy enabled", "allow_writes: true\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfig([]byte(testYAML + tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ReadOnly != tc.readOnly {
				t.Fatalf("read_only=%v", cfg.ReadOnly)
			}
		})
	}
	for _, extra := range []string{
		"read_only: null\n", "allow_writes: null\n", "read_only: invalid\n",
		"read_only: true\nallow_writes: false\n", "read_only: false\nallow_writes: false\n",
		"<<: {read_only: true}\nallow_writes: true\n",
	} {
		if _, err := ParseConfig([]byte(testYAML + extra)); err == nil {
			t.Fatalf("ambiguous policy accepted: %s", extra)
		}
	}
}

// TestModbusWriteForwardingPolicy checks what reaches the loopback emulator,
// what response the downstream client sees and when the cache is updated.
func TestModbusWriteForwardingPolicy(t *testing.T) {
	for _, readOnly := range []bool{true, false} {
		t.Run(map[bool]string{true: "blocked", false: "enabled"}[readOnly], func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			writes := make(chan []byte, 4)
			fixtureDone := make(chan struct{})
			fixtureCtx, stopFixture := context.WithCancel(context.Background())
			go func() {
				defer close(fixtureDone)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				done := make(chan struct{})
				defer close(done)
				go func() {
					select {
					case <-fixtureCtx.Done():
						conn.Close()
					case <-done:
					}
				}()
				for {
					header := make([]byte, 7)
					if _, err := io.ReadFull(conn, header); err != nil {
						return
					}
					pdu := make([]byte, int(binary.BigEndian.Uint16(header[4:6]))-1)
					if _, err := io.ReadFull(conn, pdu); err != nil {
						return
					}
					writes <- append([]byte(nil), pdu...)
					response := pdu
					// A multiple-write acknowledgement echoes the address and count.
					if pdu[0] == 0x10 {
						response = pdu[:5]
					}
					binary.BigEndian.PutUint16(header[4:6], uint16(len(response)+1))
					if _, err := conn.Write(append(header, response...)); err != nil {
						return
					}
				}
			}()
			defer func() { stopFixture(); listener.Close(); <-fixtureDone }()
			cfg := testConfig(t)
			cfg.ReadOnly = readOnly
			cfg.Inverter.Port = listener.Addr().(*net.TCPAddr).Port
			client, err := NewInverterClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			cache := testCache(t)
			server := NewServer(cfg, cache)
			server.SetInverterClient(client)
			a, b := net.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { server.handleConnection(ctx, a); close(done) }()
			defer func() { cancel(); b.Close(); <-done }()
			for _, pdu := range [][]byte{{6, 0x9c, 0xbd, 0, 42}, {0x10, 0x9c, 0xbe, 0, 2, 4, 0, 17, 0, 18}} {
				b.SetDeadline(time.Now().Add(2 * time.Second))
				header := []byte{0, 11, 0, 0, 0, byte(len(pdu) + 1), 1}
				if _, err := b.Write(append(header, pdu...)); err != nil {
					t.Fatal(err)
				}
				if _, err := io.ReadFull(b, header); err != nil {
					t.Fatal(err)
				}
				response := make([]byte, int(binary.BigEndian.Uint16(header[4:6]))-1)
				if _, err := io.ReadFull(b, response); err != nil {
					t.Fatal(err)
				}
				if readOnly {
					if !bytes.Equal(response, []byte{pdu[0] | 0x80, 1}) {
						t.Fatalf("write not blocked: %x", response)
					}
				} else {
					if !bytes.Equal(response, pdu[:5]) {
						t.Fatalf("incorrect write acknowledgement: %x", response)
					}
					select {
					case actual := <-writes:
						if !bytes.Equal(actual, pdu) {
							t.Fatalf("upstream PDU mismatch: %x", actual)
						}
					case <-time.After(time.Second):
						t.Fatal("not forwarded")
					}
				}
			}
			if readOnly {
				select {
				case <-writes:
					t.Fatal("protected mode contacted write target")
				default:
				}
				if cache.Get(1, 40125, 1) != nil {
					t.Fatal("protected mode changed cache")
				}
			} else {
				if v := cache.Get(1, 40125, 3); !bytes.Equal(encodeRegisters(v), []byte{0, 42, 0, 17, 0, 18}) {
					t.Fatalf("write cache incorrect: %v", v)
				}
			}
		})
	}
}

func encodeRegisters(values []uint16) []byte {
	data := make([]byte, 2*len(values))
	for i, v := range values {
		binary.BigEndian.PutUint16(data[i*2:], v)
	}
	return data
}

// TestReadOnlyClientDefense exercises read-only enforcement on direct calls
// to the upstream client.
func TestReadOnlyClientDefense(t *testing.T) {
	cfg := testConfig(t)
	client, err := NewInverterClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, singleErr := client.WriteSingleRegister(1, 40125, 42)
	_, multiErr := client.WriteMultipleRegisters(1, 40125, 1, []byte{0, 42})
	_, _, privateErr := client.ForwardPDU(1, []byte{0x41, 0x24, 1, 0})
	for _, err := range []error{singleErr, multiErr, privateErr} {
		if err == nil || !strings.Contains(err.Error(), "read_only") {
			t.Fatal("client-level write protection failed", err)
		}
	}
	if client.ConnectionGeneration() != 0 {
		t.Fatal("protected calls connected to inverter")
	}
}

// TestInverterWriteExceptionsPreserved verifies that both write functions
// relay upstream exception codes and leave the cache untouched on denial.
func TestInverterWriteExceptionsPreserved(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		for _, code := range []byte{2, 0x80, 2, 0x80} {
			head := make([]byte, 7)
			if _, err := io.ReadFull(conn, head); err != nil {
				return
			}
			pdu := make([]byte, int(binary.BigEndian.Uint16(head[4:6]))-1)
			if _, err := io.ReadFull(conn, pdu); err != nil {
				return
			}
			sendException(conn, binary.BigEndian.Uint16(head[:2]), head[6], pdu[0], code)
		}
	}()
	cfg := testConfig(t)
	cfg.ReadOnly = false
	cfg.Inverter.WaitAfterConnectMs = 0
	cfg.Inverter.Port = listener.Addr().(*net.TCPAddr).Port
	client, err := NewInverterClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	cache := testCache(t)
	server := NewServer(cfg, cache)
	server.SetInverterClient(client)
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan struct{})
	go func() { server.handleConnection(ctx, a); close(serverDone) }()
	defer func() { cancel(); b.Close(); <-serverDone }()
	for _, pdu := range [][]byte{{6, 0x9c, 0xbd, 0, 42}, {0x10, 0x9c, 0xbd, 0, 1, 2, 0, 42}} {
		for _, code := range []byte{2, 0x80} {
			b.SetDeadline(time.Now().Add(2 * time.Second))
			head := []byte{0, 1, 0, 0, 0, byte(len(pdu) + 1), 1}
			if _, err := b.Write(append(head, pdu...)); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(b, head); err != nil {
				t.Fatal(err)
			}
			response := make([]byte, 2)
			if _, err := io.ReadFull(b, response); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(response, []byte{pdu[0] | 0x80, code}) {
				t.Fatalf("inverter exception changed: %x", response)
			}
		}
	}
	if cache.Get(1, 40125, 1) != nil {
		t.Fatal("denied write updated cache")
	}
	<-done
}
