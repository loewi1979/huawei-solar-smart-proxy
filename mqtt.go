package main

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"
)

type MQTTConfig struct {
	Enabled   bool   `yaml:"enabled"`
	Embedded  bool   `yaml:"embedded"`
	Listen    string `yaml:"listen"`
	Broker    string `yaml:"broker"`
	Username  string `yaml:"username"`
	Password  string `yaml:"password"`
	Topic     string `yaml:"topic"`
	UnitID    byte   `yaml:"unit_id"`
	IntervalS int    `yaml:"interval_s"`
	MaxAgeS   int    `yaml:"max_age_s"`
	TLSCert   string `yaml:"tls_cert"`
	TLSKey    string `yaml:"tls_key"`
}

func defaultMQTTConfig() MQTTConfig {
	return MQTTConfig{Listen: "127.0.0.1:1883", Broker: "tcp://127.0.0.1:1883", Topic: "huawei_wechselrichter", UnitID: 1, IntervalS: 5, MaxAgeS: 30}
}
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
		if len(m.Password) < 16 || m.Username == "" {
			return fmt.Errorf("embedded MQTT requires username and password of at least 16 characters")
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

// Every remote client is a read-only subscriber; only the inline publisher can
// produce data. Exact string comparisons avoid wildcard credential semantics.
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
	return !write && (topic == h.cfg.Topic || topic == h.cfg.Topic+"/status")
}

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

// Only measured total active power (W) and lifetime energy (kWh). In particular,
// total PF cannot establish individual phase active powers or phase energies.
func victronPayload(c *RegisterCache, m MQTTConfig, now time.Time) ([]byte, error) {
	power, _, err := c.Fresh(m.UnitID, 32080, 2, time.Duration(m.MaxAgeS)*time.Second, now)
	if err != nil {
		return nil, err
	}
	energy, _, err := c.Fresh(m.UnitID, 32106, 2, time.Duration(m.MaxAgeS)*time.Second, now)
	if err != nil {
		return nil, err
	}
	p := uint32(power[0])<<16 | uint32(power[1])
	e := uint32(energy[0])<<16 | uint32(energy[1])
	if p == 0x80000000 || p == 0x7fffffff || e == 0xffffffff {
		return nil, fmt.Errorf("Huawei invalid measurement sentinel")
	}
	return json.Marshal(map[string]any{"pv": map[string]any{"power": int32(p), "energy_forward": float64(e) / 100}})
}

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
