package imageupdates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigOptInStrictYAMLAndSecrets(t *testing.T) {
	root := t.TempDir()
	smtp := filepath.Join(root, "smtp.yml")
	mqtt := filepath.Join(root, "mqtt.yml")
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := Load("false", "6h", smtp, mqtt)
	if err != nil || c.Enabled {
		t.Fatal("disabled checks must not require notification files")
	}
	write(smtp, "host: smtp.example\nuser: sender@example.com\npassword: sensitive-password\nto: [admin@example.com]\n")
	write(mqtt, "broker: ssl://mqtt.example:8883\ntopic: docklight/updates\n")
	c, err = Load("true", "6h", smtp, mqtt)
	if err != nil || !c.Enabled || c.Interval != 6*time.Hour || !c.SMTP.StartTLS || c.SMTP.From != "sender@example.com" {
		t.Fatalf("load %+v %v", c, err)
	}
	for _, bad := range []string{"password: [sensitive-password]", "unknown: sensitive-password", "host: smtp.example\n---\npassword: sensitive-password", "", "{}", "null"} {
		write(smtp, bad)
		_, err = Load("true", "6h", smtp, mqtt)
		if err == nil || strings.Contains(err.Error(), "sensitive-password") {
			t.Fatalf("invalid/secret-bearing error: %v", err)
		}
	}
	write(smtp, "host: smtp.example\nto: [admin@example.com]\nstarttls: false\n")
	if _, err = Load("true", "6h", smtp, mqtt); err == nil {
		t.Fatal("plaintext SMTP accepted")
	}
	if _, err = Load("true", "1s", smtp, mqtt); err == nil {
		t.Fatal("unsafe poll interval accepted")
	}
	if _, err = Load("false", "6h", smtp, mqtt); err != nil {
		t.Fatal("disabled checks read invalid notification configuration")
	}
}

func TestConfigEnvironmentValidation(t *testing.T) {
	for _, tc := range []struct{ enabled, interval, errorField string }{
		{"true", "1m", ""}, {"true", "168h", ""}, {"false", "6h", ""},
		{"sensitive-value", "6h", "IMAGE_UPDATES_ENABLED"},
		{"true", "sensitive-value", "IMAGE_UPDATES_INTERVAL"},
		{"true", "0", "IMAGE_UPDATES_INTERVAL"}, {"true", "-1h", "IMAGE_UPDATES_INTERVAL"},
		{"true", "168h1s", "IMAGE_UPDATES_INTERVAL"}, {"false", "1s", "IMAGE_UPDATES_INTERVAL"},
	} {
		t.Run(tc.enabled+"/"+tc.interval, func(t *testing.T) {
			c, err := Load(tc.enabled, tc.interval, filepath.Join(t.TempDir(), "smtp.yml"), filepath.Join(t.TempDir(), "mqtt.yml"))
			if tc.errorField != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errorField) || strings.Contains(err.Error(), "sensitive-value") {
					t.Fatalf("invalid or unsanitized environment error: %v", err)
				}
				return
			}
			if err != nil || c.Enabled != (tc.enabled == "true") {
				t.Fatalf("valid environment rejected: %v", err)
			}
		})
	}
}

func TestNotificationValidation(t *testing.T) {
	for _, c := range []SMTPConfig{
		{Host: "mail.example", Port: 587, To: []string{"good@example.com\r\nBcc: bad@example.com"}, StartTLS: true},
		{Host: "mail.example", Port: 70000, To: []string{"good@example.com"}, StartTLS: true},
		{Host: "mail.example", Port: 587, From: "display <good@example.com>", To: []string{"good@example.com"}, StartTLS: true},
		{Password: "secret"},
		{Host: "mail.example", Port: 587, To: []string{"good@example.com"}, StartTLS: true, CAFile: "missing.pem"},
	} {
		if c.validate() == nil {
			t.Fatalf("unsafe SMTP accepted %+v", c)
		}
	}
	for _, broker := range []string{"http://mqtt.example:80", "ssl://user:secret@mqtt.example:8883", "ssl://mqtt.example:8883/path", "ssl://mqtt.example", "ssl://mqtt.example:0", "ssl://mqtt.example:8883?token=secret"} {
		if (MQTTConfig{Broker: broker, Topic: "updates"}).validate() == nil {
			t.Fatalf("unsafe broker %s", broker)
		}
	}
	for _, topic := range []string{"", "updates/#", "updates/+", "bad\x00topic"} {
		if (MQTTConfig{Broker: "ssl://mqtt.example:8883", Topic: topic}).validate() == nil {
			t.Fatalf("unsafe topic %q", topic)
		}
	}
}
