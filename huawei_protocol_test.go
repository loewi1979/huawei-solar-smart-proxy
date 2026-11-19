package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

// TestHuaweiIdentificationAndLoginPolicy records which requests reach the
// loopback fixture and checks how the server handles its login responses.
func TestHuaweiIdentificationAndLoginPolicy(t *testing.T) {
	for _, readOnly := range []bool{true, false} {
		t.Run(map[bool]string{true: "read-only", false: "write-enabled"}[readOnly], func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			seen := make(chan []byte, 8)
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
					head := make([]byte, 7)
					if _, err := io.ReadFull(conn, head); err != nil {
						return
					}
					pdu := make([]byte, int(binary.BigEndian.Uint16(head[4:6]))-1)
					if _, err := io.ReadFull(conn, pdu); err != nil {
						return
					}
					seen <- pdu
					var response []byte
					switch pdu[0] {
					case 0x2b:
						response = []byte{0x2b, 0x0e, 1, 1, 0, 0, 1, 0, 3, 'A', 'B', 'C'}
					case 0x41:
						if pdu[1] == 0x24 {
							response = append([]byte{0x41, 0x24, 17}, make([]byte, 17)...)
						} else {
							response = append([]byte{0x41, 0x25, 34, 0, 32}, make([]byte, 32)...)
						}
					}
					binary.BigEndian.PutUint16(head[4:6], uint16(len(response)+1))
					if _, err := conn.Write(append(head, response...)); err != nil {
						return
					}
				}
			}()
			defer func() { stopFixture(); listener.Close(); <-fixtureDone }()
			cfg := testConfig(t)
			cfg.ReadOnly = readOnly
			cfg.Inverter.WaitAfterConnectMs = 50
			cfg.Inverter.Port = listener.Addr().(*net.TCPAddr).Port
			client, err := NewInverterClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			server := NewServer(cfg, testCache(t))
			server.SetInverterClient(client)
			a, b := net.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { server.handleConnection(ctx, a); close(done) }()
			defer func() { cancel(); b.Close(); <-done }()
			request := func(pdu []byte) ([]byte, error) {
				b.SetDeadline(time.Now().Add(2 * time.Second))
				head := []byte{0, 7, 0, 0, 0, byte(len(pdu) + 1), 1}
				if _, err := b.Write(append(head, pdu...)); err != nil {
					return nil, err
				}
				if _, err := io.ReadFull(b, head); err != nil {
					return nil, err
				}
				response := make([]byte, int(binary.BigEndian.Uint16(head[4:6]))-1)
				_, err := io.ReadFull(b, response)
				return response, err
			}
			identity := []byte{0x2b, 0x0e, 1, 0}
			response, err := request(identity)
			if err != nil || len(response) != 12 || !bytes.Equal(response[9:], []byte("ABC")) {
				t.Fatalf("device identification failed: %x %v", response, err)
			}
			if actual := <-seen; !bytes.Equal(actual, identity) {
				t.Fatal("identity request changed")
			}
			challenge := []byte{0x41, 0x24, 1, 0}
			login := append([]byte{0x41, 0x25, 51}, make([]byte, 16)...)
			login = append(login, 1, 'x', 32)
			login = append(login, make([]byte, 32)...)
			for _, pdu := range [][]byte{challenge, login} {
				response, err = request(pdu)
				if err != nil {
					t.Fatal(err)
				}
				if readOnly {
					if !bytes.Equal(response, []byte{0xc1, 1}) {
						t.Fatalf("login not blocked: %x", response)
					}
				} else {
					if response[0] != 0x41 || response[1] != pdu[1] {
						t.Fatalf("login response changed: %x", response)
					}
					if actual := <-seen; !bytes.Equal(actual, pdu) {
						t.Fatal("login request changed")
					}
				}
			}
			if response, err = request([]byte{0x41, 5, 0}); err != nil || !bytes.Equal(response, []byte{0xc1, 1}) {
				t.Fatal("private non-login command accepted")
			}
			select {
			case pdu := <-seen:
				t.Fatalf("forbidden request reached upstream: %x", pdu)
			default:
			}
			if !readOnly {
				// Losing the upstream session forces the authenticated downstream
				// connection to close on its next request.
				client.Reconnect()
				if _, err = request([]byte{3, 0x7d, 0x50, 0, 2}); err == nil {
					t.Fatal("authenticated client survived upstream session loss")
				}
			}
		})
	}
}

// TestConnectQuietPeriod measures the delay from accept to the first request
// on the initial connection and again after an explicit reconnect.
func TestConnectQuietPeriod(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	delays := make(chan time.Duration, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2; i++ {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			started := time.Now()
			head := make([]byte, 7)
			if _, err = io.ReadFull(conn, head); err != nil {
				conn.Close()
				return
			}
			pdu := make([]byte, int(binary.BigEndian.Uint16(head[4:6]))-1)
			if _, err = io.ReadFull(conn, pdu); err != nil {
				conn.Close()
				return
			}
			delays <- time.Since(started)
			sendReadResponse(conn, binary.BigEndian.Uint16(head[:2]), head[6], 3, []byte{0, 1})
			conn.Close()
		}
	}()
	cfg := testConfig(t)
	cfg.Inverter.WaitAfterConnectMs = 60
	cfg.Inverter.Port = listener.Addr().(*net.TCPAddr).Port
	client, err := NewInverterClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for i := 0; i < 2; i++ {
		if _, err := client.ReadRegisters(1, 32080, 1); err != nil {
			t.Fatal(err)
		}
		if delay := <-delays; delay < 40*time.Millisecond {
			t.Fatalf("connection quiet period omitted: %v", delay)
		}
		client.Reconnect()
	}
	<-done
}
