package imageupdates

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/smtp"
	"net/url"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/google/uuid"
)

func (c SMTPConfig) Send(ctx context.Context, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	message, err := c.message(events)
	if err != nil {
		return errors.New("SMTP message encoding failed")
	}
	tlsConfig, err := TLSConfig(c.Host, c.CAFile)
	if err != nil {
		return err
	}
	// Explicit SMTP-only opt-in; TLS remains required, but server identity is not verified.
	tlsConfig.InsecureSkipVerify = c.InsecureSkipVerify
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", c.address())
	if err != nil {
		return errors.New("SMTP connection failed")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	if c.SSL {
		secured := tls.Client(conn, tlsConfig)
		if err = secured.HandshakeContext(ctx); err != nil {
			return errors.New("SMTP TLS handshake failed")
		}
		client, clientErr := smtp.NewClient(secured, c.Host)
		if clientErr != nil {
			return errors.New("SMTP greeting failed")
		}
		defer client.Close()
		return c.submit(client, message)
	}
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		return errors.New("SMTP greeting failed")
	}
	defer client.Close()
	if err = client.StartTLS(tlsConfig); err != nil {
		return errors.New("SMTP STARTTLS failed")
	}
	return c.submit(client, message)
}

func (c SMTPConfig) submit(client *smtp.Client, message []byte) error {
	if c.User != "" {
		if err := client.Auth(smtp.PlainAuth("", c.User, c.Password, c.Host)); err != nil {
			return errors.New("SMTP authentication failed")
		}
	}
	if err := client.Mail(c.From); err != nil {
		return errors.New("SMTP sender rejected")
	}
	// Do not send DATA if even one recipient was refused.
	for _, to := range c.To {
		if err := client.Rcpt(to); err != nil {
			return errors.New("SMTP recipient rejected")
		}
	}
	w, err := client.Data()
	if err != nil {
		return errors.New("SMTP data rejected")
	}
	if _, err = w.Write(message); err != nil {
		_ = w.Close()
		return errors.New("SMTP message write failed")
	}
	if err = w.Close(); err != nil {
		return errors.New("SMTP message not accepted")
	}
	// DATA acceptance is success; a lost QUIT response must not resend accepted mail.
	_ = client.Quit()
	return nil
}

func (c MQTTConfig) Send(ctx context.Context, event Event) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	opts := mqtt.NewClientOptions().AddBroker(c.Broker).SetClientID("docklight-" + uuid.NewString()).SetUsername(c.User).SetPassword(c.Password).SetCleanSession(true).SetAutoReconnect(false).SetConnectRetry(false).SetConnectTimeout(10 * time.Second).SetWriteTimeout(10 * time.Second).SetKeepAlive(15 * time.Second).SetOrderMatters(false)
	broker, err := url.Parse(c.Broker)
	if err != nil {
		return errors.New("Invalid MQTT broker")
	}
	var conn net.Conn
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if broker.Scheme == "ssl" {
		tlsConfig, e := TLSConfig(broker.Hostname(), c.CAFile)
		if e != nil {
			return e
		}
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(ctx, "tcp", broker.Host)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", broker.Host)
	}
	if err != nil {
		return errors.New("MQTT connection failed")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	// Own the socket lifetime so cancellation also interrupts CONNECT and a pending PUBACK.
	opts.SetCustomOpenConnectionFn(func(*url.URL, mqtt.ClientOptions) (net.Conn, error) { return conn, nil })
	client := mqtt.NewClient(opts)
	defer client.Disconnect(0)
	if err = waitMQTT(ctx, client.Connect()); err != nil {
		return errors.New("MQTT connection failed")
	}
	if err = waitMQTT(ctx, client.Publish(c.Topic, 1, false, payload)); err != nil {
		return errors.New("MQTT publish not acknowledged")
	}
	return nil
}

func waitMQTT(ctx context.Context, token mqtt.Token) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-token.Done():
		return token.Error()
	}
}
