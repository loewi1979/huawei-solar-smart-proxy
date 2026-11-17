package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/listeners"
)

const testYAML = `inverter:
  host: 127.0.0.1
  port: 15020
  timeout_ms: 100
polling:
  read_pause_ms: 100
  fast_interval_s: 1
  fast_max_age_s: 2
  slow_interval_s: 3
  slow_max_age_s: 6
register_groups:
  - {name: power, address: 32080, count: 2, poll: fast, priority: 10}
  - {name: energy, address: 32106, count: 2, poll: fast}
  - {name: info, address: 30000, count: 2, poll: slow}
`

func testConfig(t *testing.T) *Config {
	t.Helper()
	c, err := ParseConfig([]byte(testYAML))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func testCache(t *testing.T) *RegisterCache {
	t.Helper()
	c, err := NewRegisterCache(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestConfigValidation(t *testing.T) {
	for _, path := range []string{"config.example.yaml", "config.sun2000-m1.example.yaml"} {
		if _, err := LoadConfig(path); err != nil {
			t.Fatal(path, err)
		}
	}
	for _, fragment := range []string{"typo: 1\n", "---\ninverter: {}\n", "allow_writes: nonsense\n"} {
		if _, err := ParseConfig([]byte(testYAML + fragment)); err == nil {
			t.Fatalf("accepted %q", fragment)
		}
	}
	for name, edit := range map[string]func(*Config){
		"unit zero":     func(c *Config) { c.Inverter.UnitIDs = []byte{0} },
		"zero interval": func(c *Config) { c.Polling.FastIntervalS = 0 },
		"overflow":      func(c *Config) { c.RegisterGroups[0].Address = 65535 },
		"overlap":       func(c *Config) { c.RegisterGroups[1].Address = 32081 },
		"remote web":    func(c *Config) { c.Web.Listen = ":8080" },
		"mqtt wildcard": func(c *Config) { c.MQTT.Enabled = true; c.MQTT.Topic = "#" },
	} {
		t.Run(name, func(t *testing.T) {
			c := testConfig(t)
			edit(c)
			if c.Validate() == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}

// This fixture represents an independently managed external broker which
// authorizes the proxy to publish, rather than the embedded subscriber-only ACL.
type externalFixtureAuth struct{ subscriberAuth }

func (h *externalFixtureAuth) OnACLCheck(_ *mochi.Client, topic string, _ bool) bool {
	return topic == h.cfg.Topic || topic == h.cfg.Topic+"/status"
}

func TestExternalMQTTPublish(t *testing.T) {
	m := defaultMQTTConfig()
	m.Username = "fixture"
	m.Password = "test-fixture-password"
	m.IntervalS = 1
	m.MaxAgeS = 1
	b := mochi.New(&mochi.Options{InlineClient: true})
	defer b.Close()
	if err := b.AddHook(&externalFixtureAuth{subscriberAuth: subscriberAuth{cfg: m}}, nil); err != nil {
		t.Fatal(err)
	}
	listener := listeners.NewTCP(listeners.Config{ID: "external", Address: "127.0.0.1:0"})
	if err := b.AddListener(listener); err != nil {
		t.Fatal(err)
	}
	if err := b.Serve(); err != nil {
		t.Fatal(err)
	}
	m.Broker = "tcp://" + listener.Address()
	s := paho.NewClient(paho.NewClientOptions().AddBroker(m.Broker).SetClientID("external-subscriber").SetUsername(m.Username).SetPassword(m.Password))
	token := s.Connect()
	if !token.WaitTimeout(2*time.Second) || token.Error() != nil {
		t.Fatal("connect failed", token.Error())
	}
	defer s.Disconnect(0)
	messages := make(chan paho.Message, 10)
	token = s.Subscribe(m.Topic, 0, func(_ paho.Client, msg paho.Message) {
		if len(msg.Payload()) > 0 {
			messages <- msg
		}
	})
	if !token.WaitTimeout(time.Second) || token.Error() != nil {
		t.Fatal("subscribe failed")
	}
	c := testCache(t)
	c.Set(1, 32080, []uint16{0, 987})
	c.Set(1, 32106, []uint16{0, 123})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runMQTT(ctx, c, m, nil); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case msg := <-messages:
		if !strings.Contains(string(msg.Payload()), "987") || msg.Retained() {
			t.Fatal("wrong external payload")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("external publisher failed")
	}
	time.Sleep(1200 * time.Millisecond)
	select {
	case <-messages:
		t.Fatal("external publisher sent stale data")
	default:
	}
}

func TestFreshnessAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	c, err := NewRegisterCache(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	c.BatchSet([]PendingWrite{{UnitID: 1, Address: 32080, Values: []uint16{0, 123}, UpdatedAt: now.Add(-time.Second)}})
	if _, _, err := c.Fresh(1, 32080, 2, 2*time.Second, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Fresh(2, 32080, 2, 2*time.Second, now); err == nil {
		t.Fatal("unit leaked")
	}
	if _, _, err := c.Fresh(1, 32080, 3, 2*time.Second, now); err == nil {
		t.Fatal("partial range accepted")
	}
	c.Close()
	c, err = NewRegisterCache(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, _, err := c.Fresh(1, 32080, 2, 2*time.Second, now.Add(3*time.Second)); err == nil {
		t.Fatal("persisted stale data served")
	}
	if _, _, err := c.Fresh(1, 32080, 2, 2*time.Second, now.Add(-3*time.Second)); err == nil {
		t.Fatal("future timestamp accepted")
	}
	if c.Get(1, 32080, 2) == nil {
		t.Fatal("retention incorrectly tied to freshness")
	}
}

func TestModbusStaleAndWriteProtection(t *testing.T) {
	c := testCache(t)
	cfg := testConfig(t)
	s := NewServer(cfg, c)
	c.BatchSet([]PendingWrite{{UnitID: 1, Address: 32080, Values: []uint16{0, 123}, UpdatedAt: time.Now().Add(-10 * time.Second)}})
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer b.Close()
	go s.handleConnection(ctx, a)
	request := func(pdu []byte) []byte {
		b.SetDeadline(time.Now().Add(time.Second))
		frame := []byte{0, 9, 0, 0, 0, byte(len(pdu) + 1), 1}
		frame = append(frame, pdu...)
		if _, err := b.Write(frame); err != nil {
			t.Fatal(err)
		}
		head := make([]byte, 7)
		if _, err := io.ReadFull(b, head); err != nil {
			t.Fatal(err)
		}
		response := make([]byte, int(binary.BigEndian.Uint16(head[4:6]))-1)
		if _, err := io.ReadFull(b, response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	if r := request([]byte{3, 0x7d, 0x50, 0, 2}); len(r) != 2 || r[1] != 0x0b {
		t.Fatalf("stale result: %v", r)
	}
	c.Set(1, 32080, []uint16{0, 123})
	if r := request([]byte{3, 0x7d, 0x50, 0, 2}); len(r) != 6 || r[5] != 123 {
		t.Fatalf("fresh result: %v", r)
	}
	if r := request([]byte{6, 0x7d, 0x50, 0, 2}); r[1] != 1 {
		t.Fatalf("writes not disabled: %v", r)
	}
	if r := request([]byte{3, 255, 255, 0, 2}); r[1] != 3 {
		t.Fatalf("overflow accepted: %v", r)
	}
}

func TestVictronPayload(t *testing.T) {
	c := testCache(t)
	m := defaultMQTTConfig()
	now := time.Now()
	if _, err := victronPayload(c, m, now); err == nil {
		t.Fatal("missing sample published")
	}
	c.Set(1, 32080, []uint16{0, 1234})
	c.Set(1, 32106, []uint16{0, 12345})
	data, err := victronPayload(c, m, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]map[string]float64
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result["pv"]["power"] != 1234 || result["pv"]["energy_forward"] != 123.45 || len(result["pv"]) != 2 {
		t.Fatalf("invented or wrongly scaled data: %s", data)
	}
	if _, err := victronPayload(c, m, time.Now().Add(31*time.Second)); err == nil {
		t.Fatal("stale data published")
	}
	c.Set(1, 32080, []uint16{0x8000, 0})
	if _, err := victronPayload(c, m, time.Now()); err == nil {
		t.Fatal("sentinel published")
	}
}

func TestConfigBackupRestoreRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte(testYAML)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	store := &ConfigStore{Path: path}
	if err := store.Replace([]byte("bad: 1")); err == nil {
		t.Fatal("invalid restore accepted")
	}
	changed := []byte(testYAML + "log_level: debug\n")
	if err := store.Replace(changed); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(path)
	if string(current) != string(changed) {
		t.Fatal("restore missing")
	}
	if err := store.Rollback(); err != nil {
		t.Fatal(err)
	}
	current, _ = os.ReadFile(path)
	if string(current) != string(original) {
		t.Fatal("rollback mismatch")
	}
	w := NewWebServer(testConfig(t), testCache(t))
	w.cfg.Web.Token = strings.Repeat("x", 24)
	w.store = store
	mux := http.NewServeMux()
	w.configRoutes(mux)
	handler := w.authorized(mux)
	req := httptest.NewRequest("GET", "/api/config", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != 401 {
		t.Fatal("unauthenticated backup allowed")
	}
	req.SetBasicAuth("admin", w.cfg.Web.Token)
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != 200 || resp.Body.String() != testYAML {
		t.Fatal("backup mismatch")
	}
	req = httptest.NewRequest("POST", "/api/config", strings.NewReader(testYAML))
	req.SetBasicAuth("admin", w.cfg.Web.Token)
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != 403 {
		t.Fatal("cross-origin form allowed")
	}
}

// A real TCP fixture exercises goburrow framing, reconnect after connection
// loss, polling, SQLite timestamps and the no-upstream-access-on-cache-hit path.
func TestSimulatorFailureRecovery(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var peers sync.WaitGroup
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			peers.Add(1)
			go func() {
				defer peers.Done()
				defer conn.Close()
				for {
					conn.SetDeadline(time.Now().Add(5 * time.Second))
					header := make([]byte, 7)
					if _, err := io.ReadFull(conn, header); err != nil {
						return
					}
					pdu := make([]byte, int(binary.BigEndian.Uint16(header[4:6]))-1)
					if _, err := io.ReadFull(conn, pdu); err != nil {
						return
					}
					count := binary.BigEndian.Uint16(pdu[3:5])
					data := make([]byte, int(count)*2)
					binary.BigEndian.PutUint16(data[len(data)-2:], 4321)
					sendReadResponse(conn, binary.BigEndian.Uint16(header[:2]), header[6], pdu[0], data)
				}
			}()
		}
	}()
	cfg := testConfig(t)
	cfg.Inverter.Port = listener.Addr().(*net.TCPAddr).Port
	c := testCache(t)
	client, err := NewInverterClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reader := NewReader(cfg, client, c)
	done := make(chan struct{})
	go func() { reader.Run(ctx); close(done) }()
	deadline := time.Now().Add(4 * time.Second)
	for {
		_, _, err = c.Fresh(1, 32080, 2, time.Second, time.Now())
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no simulator data")
		}
		time.Sleep(10 * time.Millisecond)
	}
	client.Reconnect() // force a new TCP session while polling is active
	time.Sleep(1500 * time.Millisecond)
	if _, _, err := c.Fresh(1, 32080, 2, time.Second, time.Now()); err != nil {
		t.Fatal("did not reconnect", err)
	}
	cancel()
	<-done
	client.Close()
	listener.Close()
	peers.Wait()
}

func TestEmbeddedBrokerAuthAndStaleSilence(t *testing.T) {
	m := defaultMQTTConfig()
	m.Enabled = true
	m.Embedded = true
	m.Listen = "127.0.0.1:0"
	m.Username = "gx"
	m.Password = "test-only-password-123456"
	m.IntervalS = 1
	m.MaxAgeS = 1
	b, err := startBroker(m)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	listener, ok := b.Listeners.Get("mqtt")
	if !ok {
		t.Fatal("broker listener missing")
	}
	addr := listener.Address()
	opts := paho.NewClientOptions().AddBroker("tcp://" + addr).SetClientID("test-subscriber").SetUsername(m.Username).SetPassword(m.Password).SetConnectTimeout(time.Second)
	subscriber := paho.NewClient(opts)
	token := subscriber.Connect()
	if !token.WaitTimeout(2*time.Second) || token.Error() != nil {
		t.Fatal("subscriber connect failed", token.Error())
	}
	defer subscriber.Disconnect(0)
	messages := make(chan paho.Message, 10)
	token = subscriber.Subscribe(m.Topic, 0, func(_ paho.Client, msg paho.Message) { messages <- msg })
	if !token.WaitTimeout(time.Second) || token.Error() != nil {
		t.Fatal("subscribe failed")
	}
	c := testCache(t)
	c.Set(1, 32080, []uint16{0, 42})
	c.Set(1, 32106, []uint16{0, 123})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runMQTT(ctx, c, m, b); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case msg := <-messages:
		if msg.Retained() {
			t.Fatal("retained telemetry")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no MQTT payload")
	}
	time.Sleep(1200 * time.Millisecond)
	select {
	case <-messages:
		t.Fatal("stale sample republished")
	default:
	}
	denied := paho.NewClient(paho.NewClientOptions().AddBroker("tcp://" + addr).SetClientID("unauthorized").SetUsername("bad").SetPassword("bad").SetConnectTimeout(time.Second))
	token = denied.Connect()
	if !token.WaitTimeout(2*time.Second) || token.Error() == nil {
		t.Fatal("bad credentials accepted")
	}
}

func TestFastPollingPreemptsSlowBacklog(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	observed := make(chan uint16, 64)
	fixtureDone := make(chan struct{})
	go func() {
		defer close(fixtureDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			header := make([]byte, 7)
			if _, err := io.ReadFull(conn, header); err != nil {
				return
			}
			pdu := make([]byte, int(binary.BigEndian.Uint16(header[4:6]))-1)
			if _, err := io.ReadFull(conn, pdu); err != nil {
				return
			}
			observed <- binary.BigEndian.Uint16(pdu[1:3])
			sendReadResponse(conn, binary.BigEndian.Uint16(header[:2]), header[6], 3, []byte{0, 1})
		}
	}()
	cfg := testConfig(t)
	cfg.Inverter.Port = listener.Addr().(*net.TCPAddr).Port
	cfg.RegisterGroups = []RegisterGroup{{Name: "fast", Address: 32080, Count: 1, Poll: "fast", Priority: 10}}
	for i := 0; i < 20; i++ {
		cfg.RegisterGroups = append(cfg.RegisterGroups, RegisterGroup{Name: fmt.Sprint("slow", i), Address: uint16(1000 + i), Count: 1, Poll: "slow"})
	}
	c := testCache(t)
	client, err := NewInverterClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { NewReader(cfg, client, c).Run(ctx); close(done) }()
	defer func() { cancel(); <-done; client.Close(); <-fixtureDone }()
	fastCount := 0
	slowCount := 0
	deadline := time.After(4 * time.Second)
	for fastCount < 2 {
		select {
		case addr := <-observed:
			if addr == 32080 {
				fastCount++
			} else {
				slowCount++
			}
		case <-deadline:
			t.Fatal("fast polling starved by slow backlog")
		}
	}
	if slowCount == 0 || slowCount >= 20 {
		t.Fatalf("slow backlog was not preempted: %d slow reads", slowCount)
	}
}

func TestReconnectPreservesRequestGap(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.Inverter.Port = listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	client, err := NewInverterClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.ReadRegisters(1, 32080, 2); err == nil {
		t.Fatal("expected unavailable inverter")
	}
	client.Reconnect()
	started := time.Now()
	client.ReadRegisters(1, 32080, 2)
	if time.Since(started) < 80*time.Millisecond {
		t.Fatal("failed reads bypassed request pause after reconnect")
	}
}
