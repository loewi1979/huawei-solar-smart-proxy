// MQTT telemetry comes from the register cache and can use the embedded broker
// or an external broker. The publisher shares the cache with Modbus clients.

package main

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"strings"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"
)

// MQTTConfig selects a broker, a polled unit, and the publish and freshness limits.
type MQTTConfig struct {
	Enabled            bool   `yaml:"enabled"`
	Embedded           bool   `yaml:"embedded"`
	Listen             string `yaml:"listen"`
	Broker             string `yaml:"broker"`
	Username           string `yaml:"username"`
	Password           string `yaml:"password"`
	Topic              string `yaml:"topic"`
	UnitID             byte   `yaml:"unit_id"`
	IntervalS          int    `yaml:"interval_s"`
	MaxAgeS            int    `yaml:"max_age_s"`
	TLSCert            string `yaml:"tls_cert"`
	TLSKey             string `yaml:"tls_key"`
	EstimatePhasePower bool   `yaml:"estimate_phase_power"`
}

func defaultMQTTConfig() MQTTConfig {
	return MQTTConfig{Listen: "127.0.0.1:1883", Broker: "tcp://127.0.0.1:1883", Topic: "huawei_wechselrichter", UnitID: 1, IntervalS: 5, MaxAgeS: 30}
}

// Validate checks enabled MQTT settings against the configured units and the
// fast tier's maximum age. Telemetry can use a shorter freshness window.
func (m MQTTConfig) Validate(c *Config) error {
	if !m.Enabled {
		return nil
	}
	if m.IntervalS < 1 || m.MaxAgeS < m.IntervalS || m.MaxAgeS > c.Polling.FastMaxAgeS || m.Topic == "" || strings.ContainsAny(m.Topic, "+#\x00") {
		return fmt.Errorf("invalid MQTT topic/interval/freshness")
	}
	found := false
	for _, u := range c.Inverter.UnitIDs {
		if u == m.UnitID {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("MQTT unit_id must be polled")
	}
	if m.Embedded {
		if _, _, err := net.SplitHostPort(m.Listen); err != nil {
			return err
		}
		if m.Password == "" || m.Username == "" {
			return fmt.Errorf("embedded MQTT requires non-empty username and password")
		}
		if (m.TLSCert == "") != (m.TLSKey == "") {
			return fmt.Errorf("MQTT TLS requires both cert and key")
		}
	} else {
		u, err := url.Parse(m.Broker)
		if err != nil || u.Host == "" || (u.Scheme != "tcp" && u.Scheme != "ssl") || u.User != nil {
			return fmt.Errorf("MQTT broker must be tcp:// or ssl:// without URL credentials")
		}
	}
	return nil
}

// subscriberAuth accepts exact credentials and gives network clients read-only
// access to the telemetry and status topics. The in-process client publishes.
type subscriberAuth struct {
	mochi.HookBase
	cfg MQTTConfig
}

func (h *subscriberAuth) ID() string { return "proxy-subscriber-auth" }
func (h *subscriberAuth) Provides(b byte) bool {
	return b == mochi.OnConnectAuthenticate || b == mochi.OnACLCheck
}
func (h *subscriberAuth) OnConnectAuthenticate(_ *mochi.Client, p packets.Packet) bool {
	return subtle.ConstantTimeCompare(p.Connect.Username, []byte(h.cfg.Username)) == 1 && subtle.ConstantTimeCompare(p.Connect.Password, []byte(h.cfg.Password)) == 1
}
func (h *subscriberAuth) OnACLCheck(_ *mochi.Client, topic string, write bool) bool {
	if write {
		return false
	}
	// Mochi checks both subscription filters and each delivered topic. Accept
	// MQTT Explorer's broad filters and restrict delivery to the two proxy topics.
	return topic == h.cfg.Topic || topic == h.cfg.Topic+"/status" ||
		topic == h.cfg.Topic+"/#" || topic == "#" || topic == "$SYS/#"
}

// startBroker starts the embedded broker with bounded client capacity and
// optional TLS, and enables the in-process publisher used by runMQTT.
func startBroker(m MQTTConfig) (*mochi.Server, error) {
	caps := mochi.NewDefaultServerCapabilities()
	caps.MaximumClients = 64
	caps.MaximumPacketSize = 65536
	caps.MaximumSessionExpiryInterval = 0
	caps.ReceiveMaximum = 16
	b := mochi.New(&mochi.Options{InlineClient: true, Logger: slog.Default(), Capabilities: caps})
	if err := b.AddHook(&subscriberAuth{cfg: m}, nil); err != nil {
		return nil, err
	}
	lc := listeners.Config{ID: "mqtt", Address: m.Listen}
	if m.TLSCert != "" {
		cert, err := tls.LoadX509KeyPair(m.TLSCert, m.TLSKey)
		if err != nil {
			return nil, err
		}
		lc.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	}
	if err := b.AddListener(listeners.NewTCP(lc)); err != nil {
		return nil, err
	}
	if err := b.Serve(); err != nil {
		b.Close()
		return nil, err
	}
	return b, nil
}

// victronPayload encodes inverter telemetry in the Victron PV JSON format.
// Fresh active power and lifetime yield are required. Each phase diagnostic
// uses its own freshness and validity check, so partial phase data can remain.
func victronPayload(c *RegisterCache, m MQTTConfig, now time.Time) ([]byte, error) {
	power, _, err := c.Fresh(m.UnitID, 32080, 2, time.Duration(m.MaxAgeS)*time.Second, now)
	if err != nil {
		return nil, err
	}
	energy, _, err := c.Fresh(m.UnitID, 32106, 2, time.Duration(m.MaxAgeS)*time.Second, now)
	if err != nil {
		return nil, err
	}
	// Huawei puts the high word first. Power is signed and already in watts,
	// while lifetime yield is unsigned with a resolution of 0.01 kWh.
	p := uint32(power[0])<<16 | uint32(power[1])
	e := uint32(energy[0])<<16 | uint32(energy[1])
	if p == 0x80000000 || p == 0x7fffffff || e == 0xffffffff {
		return nil, fmt.Errorf("Huawei invalid measurement sentinel")
	}
	pv := map[string]any{"power": int32(p), "energy_forward": float64(e) / 100}
	// Decode optional words using the register's signedness and gain. The
	// boolean distinguishes an unavailable diagnostic from a measured zero.
	optional := func(address, count uint16, signed bool, gain float64) (float64, bool) {
		regs, _, err := c.Fresh(m.UnitID, address, count, time.Duration(m.MaxAgeS)*time.Second, now)
		if err != nil {
			return 0, false
		}
		if count == 1 {
			if signed {
				if regs[0] == 0x8000 {
					return 0, false
				}
				return float64(int16(regs[0])) / gain, true
			}
			if regs[0] == 0xffff {
				return 0, false
			}
			return float64(regs[0]) / gain, true
		}
		raw := uint32(regs[0])<<16 | uint32(regs[1])
		if signed {
			if raw == 0x80000000 || raw == 0x7fffffff {
				return 0, false
			}
			return float64(int32(raw)) / gain, true
		}
		return float64(raw) / gain, true
	}
	// Huawei supplies one measured grid frequency, shared by all three phases.
	frequency, frequencyOK := optional(32085, 1, false, 100)
	var totalPF float64
	var totalPFOK bool
	if m.EstimatePhasePower {
		// The measured total power factor is a common input to the estimates.
		totalPF, totalPFOK = optional(32084, 1, true, 1000)
		totalPFOK = totalPFOK && math.Abs(totalPF) <= 1
	}
	for i := uint16(0); i < 3; i++ {
		phase := map[string]any{}
		// Phase voltage uses one word per phase in tenths of a volt. Signed
		// phase current uses two words per phase in milliamps.
		voltage, voltageOK := optional(32069+i, 1, false, 10)
		current, currentOK := optional(32072+2*i, 2, true, 1000)
		if voltageOK {
			phase["voltage"] = voltage
		}
		if currentOK {
			phase["current"] = current
		}
		if frequencyOK {
			phase["frequency"] = frequency
		}
		if m.EstimatePhasePower && voltageOK && currentOK && totalPFOK {
			// Estimate watts as V × A × total PF and label the result for consumers.
			phase["power"] = math.Round(voltage*current*totalPF*100) / 100
			phase["power_is_estimate"] = true
		}
		if len(phase) > 0 {
			pv[fmt.Sprintf("L%d", i+1)] = phase
		}
	}
	return json.Marshal(map[string]any{"pv": pv})
}

// runMQTT publishes at the configured interval until cancellation. Telemetry
// is non-retained. The retained status tracks publish success and shutdown.
func runMQTT(ctx context.Context, c *RegisterCache, m MQTTConfig, b *mochi.Server) {
	var client paho.Client
	if b == nil {
		opts := paho.NewClientOptions().AddBroker(m.Broker).SetClientID("huawei-smart-proxy").SetUsername(m.Username).SetPassword(m.Password).
			SetCleanSession(true).SetAutoReconnect(false).SetConnectTimeout(3*time.Second).SetWriteTimeout(3*time.Second).
			SetWill(m.Topic+"/status", "offline", 0, true)
		client = paho.NewClient(opts)
		defer client.Disconnect(0)
	}
	publish := func(topic string, data []byte, retained bool) error {
		// Recheck freshness before the embedded publish or external connection.
		if topic == m.Topic && len(data) > 0 {
			var err error
			data, err = victronPayload(c, m, time.Now())
			if err != nil {
				return err
			}
		}
		if b != nil {
			return b.Publish(topic, data, retained, 0)
		}
		if !client.IsConnected() {
			t := client.Connect()
			if !t.WaitTimeout(4 * time.Second) {
				return fmt.Errorf("MQTT connect timeout")
			}
			if t.Error() != nil {
				return t.Error()
			}
			// Remove an old retained data sample from earlier software versions.
			t = client.Publish(m.Topic, 0, true, []byte{})
			if !t.WaitTimeout(3*time.Second) || t.Error() != nil {
				client.Disconnect(0)
				return fmt.Errorf("MQTT retained cleanup failed")
			}
		}
		// External connection and retained cleanup can take several seconds.
		// Re-read measurements so a sample that expired during setup is withheld.
		if topic == m.Topic && len(data) > 0 {
			var err error
			data, err = victronPayload(c, m, time.Now())
			if err != nil {
				return err
			}
		}
		t := client.Publish(topic, 0, retained, data)
		if !t.WaitTimeout(3 * time.Second) {
			client.Disconnect(0)
			return fmt.Errorf("MQTT publish timeout")
		}
		return t.Error()
	}
	defer func() { _ = publish(m.Topic+"/status", []byte("offline"), true) }()
	ticker := time.NewTicker(time.Duration(m.IntervalS) * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		payload, err := victronPayload(c, m, time.Now())
		status := "stale"
		if err == nil {
			if err = publish(m.Topic, payload, false); err == nil {
				status = "online"
			}
		}
		if err != nil {
			slog.Warn("MQTT data withheld", "error", err)
		}
		if err := publish(m.Topic+"/status", []byte(status), true); err != nil {
			slog.Warn("MQTT unavailable", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
