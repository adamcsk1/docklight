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

func TestSMTPInsecureSkipVerifyYAML(t *testing.T) {
	for _, tc := range []struct {
		name, setting string
		want, invalid bool
	}{
		{name: "omitted"},
		{name: "false", setting: "insecure_skip_verify: false\n"},
		{name: "true", setting: "insecure_skip_verify: true\n", want: true},
		{name: "invalid", setting: "insecure_skip_verify: sensitive-value\n", invalid: true},
		{name: "tls-required", setting: "insecure_skip_verify: true\nstarttls: false\nssl: false\n", invalid: true},
		{name: "missing-ca", setting: "insecure_skip_verify: true\nca_file: missing.pem\n", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			file := filepath.Join(root, "smtp.yml")
			if err := os.WriteFile(file, []byte("host: smtp.example\nto: [admin@example.com]\n"+tc.setting), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := Load("true", "6h", file, filepath.Join(root, "mqtt.yml"))
			if tc.invalid {
				if err == nil || strings.Contains(err.Error(), "sensitive-value") {
					t.Fatalf("invalid or unsanitized SMTP option error: %v", err)
				}
				return
			}
			if err != nil || c.SMTP.InsecureSkipVerify != tc.want {
				t.Fatalf("SMTP insecure_skip_verify=%v, want %v; error=%v", c.SMTP.InsecureSkipVerify, tc.want, err)
			}
		})
	}
}

func TestSMTPInsecureSkipVerifyIsolationAndCAValidation(t *testing.T) {
	root := t.TempDir()
	smtpFile, mqttFile := filepath.Join(root, "smtp.yml"), filepath.Join(root, "mqtt.yml")
	if err := os.WriteFile(smtpFile, []byte("host: smtp.example\nto: [admin@example.com]\ninsecure_skip_verify: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mqttFile, []byte("broker: ssl://mqtt.example:8883\ntopic: updates\ninsecure_skip_verify: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("true", "6h", smtpFile, mqttFile); err == nil {
		t.Fatal("SMTP-only option accepted in MQTT configuration")
	}
	config, err := TLSConfig("mqtt.example", "")
	if err != nil || config.InsecureSkipVerify {
		t.Fatal("shared TLS helper must keep certificate verification enabled")
	}
	ca := filepath.Join(root, "invalid-ca.pem")
	if err := os.WriteFile(ca, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	c := SMTPConfig{Host: "smtp.example", Port: 587, To: []string{"admin@example.com"}, StartTLS: true, InsecureSkipVerify: true, CAFile: ca}
	if c.validate() == nil {
		t.Fatal("SMTP opt-in bypassed configured CA validation")
	}
	if err := c.Send(t.Context(), notificationEvent()); err == nil || err.Error() != "invalid notification CA certificate" {
		t.Fatalf("SMTP opt-in did not reject invalid CA before dialing: %v", err)
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
