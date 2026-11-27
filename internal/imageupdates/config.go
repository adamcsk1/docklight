// Package imageupdates checks deployed image content without pulling or deploying images.
package imageupdates

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Enabled  bool
	Interval time.Duration
	SMTP     SMTPConfig
	MQTT     MQTTConfig
}

type SMTPConfig struct {
	Host     string   `yaml:"host"`
	Port     int      `yaml:"port"`
	User     string   `yaml:"user"`
	Password string   `yaml:"password"`
	From     string   `yaml:"from"`
	To       []string `yaml:"to"`
	StartTLS bool     `yaml:"starttls"`
	SSL      bool     `yaml:"ssl"`
	CAFile   string   `yaml:"ca_file"`
}

type MQTTConfig struct {
	Broker   string `yaml:"broker"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Topic    string `yaml:"topic"`
	CAFile   string `yaml:"ca_file"`
}

func readYAML(path string, value any) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cannot open notification configuration: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > 1<<20 {
		return false, errors.New("notification configuration exceeds size limit or cannot be read")
	}
	d := yaml.NewDecoder(io.LimitReader(f, 1<<20))
	d.KnownFields(true)
	// Do not return decoder errors: they can quote secret YAML values.
	if d.Decode(value) != nil {
		return false, errors.New("invalid notification YAML configuration")
	}
	if d.Decode(new(any)) != io.EOF {
		return false, errors.New("unexpected notification configuration data")
	}
	return true, nil
}

func Load(enabled, interval, smtpFile, mqttFile string) (Config, error) {
	c := Config{SMTP: SMTPConfig{Port: 587, StartTLS: true}}
	var err error
	c.Enabled, err = strconv.ParseBool(enabled)
	if err != nil {
		return c, errors.New("IMAGE_UPDATES_ENABLED must be a valid boolean")
	}
	c.Interval, err = time.ParseDuration(interval)
	if err != nil || c.Interval < time.Minute || c.Interval > 7*24*time.Hour {
		return c, errors.New("IMAGE_UPDATES_INTERVAL must be a duration between 1m and 168h")
	}
	if !c.Enabled {
		return c, nil
	}
	smtpPresent, err := readYAML(smtpFile, &c.SMTP)
	if err != nil {
		return c, err
	}
	mqttPresent, err := readYAML(mqttFile, &c.MQTT)
	if err != nil {
		return c, err
	}
	if smtpPresent && c.SMTP.Host == "" {
		return c, errors.New("SMTP host is required in smtp.yml")
	}
	if mqttPresent && c.MQTT.Broker == "" {
		return c, errors.New("MQTT broker is required in mqtt.yml")
	}
	if err = c.SMTP.validate(); err != nil {
		return c, err
	}
	if err = c.MQTT.validate(); err != nil {
		return c, err
	}
	return c, nil
}

func (c *SMTPConfig) validate() error {
	if c.Host == "" {
		if c.User != "" || c.Password != "" || c.From != "" || len(c.To) > 0 {
			return errors.New("SMTP host is required")
		}
		return nil
	}
	if strings.ContainsAny(c.Host, "\r\n /@") || c.Port < 1 || c.Port > 65535 || len(c.To) == 0 || c.User == "" && c.Password != "" {
		return errors.New("invalid SMTP host, port, credentials or recipients")
	}
	if c.From == "" {
		c.From = c.User
		if c.From == "" {
			c.From = c.To[0]
		}
	}
	for _, address := range append([]string{c.From}, c.To...) {
		parsed, err := mail.ParseAddress(address)
		if err != nil || parsed.Address != address || strings.ContainsAny(address, "\r\n") {
			return errors.New("SMTP addresses must be plain valid email addresses")
		}
	}
	if !c.SSL && !c.StartTLS {
		return errors.New("SMTP requires STARTTLS or implicit TLS")
	}
	_, err := TLSConfig(c.Host, c.CAFile)
	return err
}

func (c MQTTConfig) validate() error {
	if c.Broker == "" {
		if c.User != "" || c.Password != "" || c.Topic != "" {
			return errors.New("MQTT broker is required")
		}
		return nil
	}
	u, err := url.Parse(c.Broker)
	if err != nil || (u.Scheme != "tcp" && u.Scheme != "ssl") || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || c.Topic == "" || len(c.Topic) > 65535 || strings.ContainsAny(c.Topic, "+#\x00") || c.User == "" && c.Password != "" {
		return errors.New("invalid MQTT broker, topic or credentials")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return errors.New("MQTT broker requires valid explicit port")
	}
	// Plain MQTT is only for an explicitly configured isolated network, never public transport.
	if u.Scheme == "ssl" {
		_, err = TLSConfig(u.Hostname(), c.CAFile)
		return err
	}
	if c.CAFile != "" {
		return errors.New("MQTT ca_file requires ssl transport")
	}
	return nil
}

func TLSConfig(host, caFile string) (*tls.Config, error) {
	c := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: strings.Trim(host, "[]")}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, errors.New("cannot read notification CA file")
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("invalid notification CA certificate")
		}
		c.RootCAs = pool
	}
	return c, nil
}

func (c SMTPConfig) address() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }
