// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SukramJ/go-mqtt"
)

// minimumYAMLNoPort is minimumYAML without MQTT_PORT, so the
// scheme-derived default applies.
const minimumYAMLNoPort = `
MODBUS_IP: 192.168.0.10
MODBUS_PORT: 502
MODBUS_SLAVE: 247
MODBUS_TIMEOUT: 5
MQTT_SERVER: broker.example
MQTT_TOPIC: MTEC
`

// writeCA writes a freshly generated self-signed CA certificate as PEM
// and returns its path.
func writeCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mtec2mqtt test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMQTTTLSDefaultsOff(t *testing.T) {
	c, err := Load(strings.NewReader(minimumYAML), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.MQTTTLS || c.MQTTTLSInsecure || c.MQTTTLSCA != "" {
		t.Errorf("TLS keys must default off: tls=%v insecure=%v ca=%q", c.MQTTTLS, c.MQTTTLSInsecure, c.MQTTTLSCA)
	}
	if c.MQTTBrokerURL() != "tcp://localhost" {
		t.Errorf("default broker URL = %q, want tcp://localhost", c.MQTTBrokerURL())
	}
	tlsConfig, err := c.MQTTTLSConfig()
	if err != nil || tlsConfig != nil {
		t.Errorf("TLS off must give no tls.Config: %v, %v", tlsConfig, err)
	}
}

func TestMQTTTLSFromYAML(t *testing.T) {
	ca := writeCA(t)
	c, err := Load(strings.NewReader(minimumYAMLNoPort+"MQTT_TLS: true\nMQTT_TLS_CA: "+ca+"\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c.MQTTTLS || c.MQTTTLSCA != ca {
		t.Fatalf("tls=%v ca=%q", c.MQTTTLS, c.MQTTTLSCA)
	}
	if c.MQTTPort != 8883 {
		t.Errorf("MQTT_PORT default with MQTT_TLS = %d, want 8883", c.MQTTPort)
	}
	if got := c.MQTTBrokerURL(); got != "tls://broker.example" {
		t.Errorf("broker URL = %q, want tls://broker.example", got)
	}
	tlsConfig, err := c.MQTTTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	if tlsConfig.ServerName != "broker.example" {
		t.Errorf("ServerName = %q, want the bare MQTT_SERVER host", tlsConfig.ServerName)
	}
	if tlsConfig.RootCAs == nil {
		t.Error("MQTT_TLS_CA must become RootCAs")
	}
	if tlsConfig.InsecureSkipVerify {
		t.Error("verification must stay on")
	}
}

func TestMQTTTLSFromEnv(t *testing.T) {
	env := fakeEnv{vars: map[string]string{
		"MTEC_MQTT_TLS":          "true",
		"MTEC_MQTT_TLS_INSECURE": "true",
	}}
	c, err := Load(strings.NewReader(minimumYAML), env)
	if err != nil {
		t.Fatal(err)
	}
	if !c.MQTTTLS || !c.MQTTTLSInsecure {
		t.Fatalf("env overrides failed: tls=%v insecure=%v", c.MQTTTLS, c.MQTTTLSInsecure)
	}
	// MQTT_PORT: 1883 is explicit in minimumYAML and survives.
	if got := c.MQTTBrokerURL(); got != "tls://localhost:1883" {
		t.Errorf("broker URL = %q, want tls://localhost:1883", got)
	}
	tlsConfig, err := c.MQTTTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !tlsConfig.InsecureSkipVerify || tlsConfig.ServerName != "localhost" {
		t.Errorf("insecure=%v serverName=%q", tlsConfig.InsecureSkipVerify, tlsConfig.ServerName)
	}

	// MQTT_TLS_CA is a string key: a path is taken verbatim.
	ca := writeCA(t)
	env = fakeEnv{vars: map[string]string{"MTEC_MQTT_TLS": "true", "MTEC_MQTT_TLS_CA": ca}}
	c, err = Load(strings.NewReader(minimumYAML), env)
	if err != nil {
		t.Fatal(err)
	}
	if c.MQTTTLSCA != ca {
		t.Errorf("MTEC_MQTT_TLS_CA = %q, want %q", c.MQTTTLSCA, ca)
	}
}

func TestMQTTPortDefaultPlain(t *testing.T) {
	c, err := Load(strings.NewReader(minimumYAMLNoPort), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.MQTTPort != 1883 {
		t.Errorf("MQTT_PORT default = %d, want 1883", c.MQTTPort)
	}
}

// An old key — file, any case, or env — must stop the daemon: silently
// ignoring MQTT_SSL: true would fall back to plain TCP.
func TestRenamedSSLKeysAreRefused(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		env  map[string]string
		want []string
	}{
		{"yaml MQTT_SSL", "MQTT_SSL: true\n", nil, []string{"MQTT_SSL was renamed to MQTT_TLS"}},
		{"yaml lower-case", "mqtt_ssl_insecure: false\n", nil, []string{"MQTT_SSL_INSECURE was renamed to MQTT_TLS_INSECURE"}},
		{"env both", "", map[string]string{"MTEC_MQTT_SSL": "true", "MTEC_MQTT_SSL_INSECURE": "true"}, []string{
			"MQTT_SSL was renamed to MQTT_TLS",
			"MQTT_SSL_INSECURE was renamed to MQTT_TLS_INSECURE",
		}},
		{"set to false still refused", "MQTT_SSL: false\n", nil, []string{"MQTT_SSL was renamed to MQTT_TLS"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(strings.NewReader(minimumYAML+tc.yaml), fakeEnv{vars: tc.env})
			if err == nil {
				t.Fatal("an old MQTT_SSL* key must refuse the configuration")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}

func TestMQTTTLSCAWithInsecureIsInvalid(t *testing.T) {
	_, err := Load(strings.NewReader(minimumYAML+"MQTT_TLS: true\nMQTT_TLS_INSECURE: true\nMQTT_TLS_CA: /ssl/ca.crt\n"), nil)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want a ValidationError, got %v", err)
	}
	if !strings.Contains(err.Error(), "MQTT_TLS_CA and MQTT_TLS_INSECURE are mutually exclusive") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestMQTTTLSConfigNamesAnUnusableCA(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.crt")
	c := &Config{MQTTServer: "broker", MQTTTLS: true, MQTTTLSCA: missing}
	_, err := c.MQTTTLSConfig()
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("want an error naming %s, got %v", missing, err)
	}

	// Belt and braces: the validation guard is the first line; the
	// library's own refusal is the second.
	c = &Config{MQTTServer: "broker", MQTTTLS: true, MQTTTLSCA: writeCA(t), MQTTTLSInsecure: true}
	if _, err := c.MQTTTLSConfig(); !errors.Is(err, mqtt.ErrInsecureWithCA) {
		t.Fatalf("want ErrInsecureWithCA, got %v", err)
	}
}

func TestIgnoredTLSKeys(t *testing.T) {
	c := &Config{MQTTTLSInsecure: true, MQTTTLSCA: "/ssl/ca.crt"}
	if got := strings.Join(c.IgnoredTLSKeys(), ","); got != "MQTT_TLS_INSECURE,MQTT_TLS_CA" {
		t.Errorf("IgnoredTLSKeys = %q", got)
	}
	c.MQTTTLS = true
	if got := c.IgnoredTLSKeys(); got != nil {
		t.Errorf("with MQTT_TLS on nothing is ignored, got %v", got)
	}
}
