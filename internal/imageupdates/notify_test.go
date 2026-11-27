package imageupdates

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testTLS(t *testing.T) (*tls.Config, string) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	config := server.TLS.Clone()
	file := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: config.Certificates[0].Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	return config, file
}

func notificationEvent() Event {
	return Event{ID: strings.Repeat("e", 64), Type: "image_update_available", Host: HostIdentity{ID: "nas", Name: "Main NAS", Hostname: "nas-main", Address: "https://192.0.2.10:8081"}, Image: "registry.example/web:latest", Platform: "linux/arm/v7", Deployed: oldSHA, Available: newSHA, Detected: time.Now().UTC(), Targets: []Target{{Container: "web-1", Stack: "web", Service: "app"}}}
}

func TestSMTPTLSDeliveryAndPartialRefusal(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		for _, mode := range []string{"ok", "recipient", "auth", "untrusted-tls", "insecure-tls", "hostname-mismatch", "insecure-hostname", "tls-unavailable"} {
			refuse := mode == "recipient"
			t.Run(strconv.FormatBool(implicit)+"/"+mode, func(t *testing.T) {
				tlsConfig, ca := testTLS(t)
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				if implicit && mode != "tls-unavailable" {
					listener = tls.NewListener(listener, tlsConfig)
				}
				messages := make(chan string, 1)
				done := make(chan struct{})
				go func() {
					defer close(done)
					conn, e := listener.Accept()
					if e != nil {
						return
					}
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
					peer := textproto.NewConn(conn)
					securedTransport := implicit && mode != "tls-unavailable"
					_ = peer.PrintfLine("220 test SMTP")
					for {
						line, e := peer.ReadLine()
						if e != nil {
							return
						}
						switch {
						case strings.HasPrefix(line, "EHLO"):
							_ = peer.PrintfLine("250-test")
							_ = peer.PrintfLine("250-AUTH PLAIN")
							_ = peer.PrintfLine("250 STARTTLS")
						case line == "STARTTLS":
							if mode == "tls-unavailable" {
								_ = peer.PrintfLine("454 TLS unavailable")
								return
							}
							_ = peer.PrintfLine("220 ready")
							secured := tls.Server(conn, tlsConfig)
							if secured.Handshake() != nil {
								return
							}
							peer = textproto.NewConn(secured)
							securedTransport = true
						case strings.HasPrefix(line, "AUTH PLAIN "):
							decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "AUTH PLAIN "))
							if !securedTransport || err != nil || string(decoded) != "\x00sender@example.com\x00test-password" {
								t.Error("incorrect or unencrypted SMTP authentication")
							}
							if mode == "auth" {
								_ = peer.PrintfLine("535 refused")
							} else {
								_ = peer.PrintfLine("235 authenticated")
							}
						case strings.HasPrefix(line, "MAIL FROM"):
							_ = peer.PrintfLine("250 sender")
						case strings.HasPrefix(line, "RCPT TO"):
							if refuse && strings.Contains(line, "second@") {
								_ = peer.PrintfLine("550 refused")
							} else {
								_ = peer.PrintfLine("250 recipient")
							}
						case line == "DATA":
							_ = peer.PrintfLine("354 send data")
							body, e := io.ReadAll(peer.DotReader())
							if e != nil {
								return
							}
							messages <- string(body)
							_ = peer.PrintfLine("250 accepted")
						case line == "QUIT":
							return // Acceptance must survive lost QUIT response.
						default:
							_ = peer.PrintfLine("500 unsupported")
						}
					}
				}()
				host, port, _ := net.SplitHostPort(listener.Addr().String())
				p, _ := strconv.Atoi(port)
				c := SMTPConfig{Host: host, Port: p, User: "sender@example.com", Password: "test-password", From: "sender@example.com", To: []string{"first@example.com", "second@example.com"}, SSL: implicit, StartTLS: !implicit, CAFile: ca}
				if mode == "untrusted-tls" || mode == "insecure-tls" {
					c.CAFile = ""
				}
				if mode == "hostname-mismatch" || mode == "insecure-hostname" {
					c.Host = "localhost"
					certificate, err := x509.ParseCertificate(tlsConfig.Certificates[0].Certificate[0])
					if err != nil || certificate.VerifyHostname(c.Host) == nil {
						t.Fatal("fixture must have a certificate with a mismatched hostname")
					}
				}
				c.InsecureSkipVerify = mode == "insecure-tls" || mode == "insecure-hostname" || mode == "tls-unavailable"
				event := notificationEvent()
				event.Host.Name += " – árvíz"
				err = c.Send(context.Background(), event)
				accepted := mode == "ok" || mode == "insecure-tls" || mode == "insecure-hostname"
				if (err == nil) != accepted {
					t.Fatalf("SMTP result %v", err)
				}
				<-done
				if !accepted {
					if len(messages) != 0 {
						t.Fatal("DATA sent despite SMTP or TLS failure")
					}
					return
				}
				wire := <-messages
				if strings.Contains(wire, "árvíz") {
					t.Fatal("non-ASCII bytes sent without negotiated 8BITMIME")
				}
				message, err := mail.ReadMessage(strings.NewReader(wire))
				if err != nil {
					t.Fatal(err)
				}
				decoded, err := io.ReadAll(quotedprintable.NewReader(message.Body))
				if err != nil {
					t.Fatal(err)
				}
				subject, err := (&mime.WordDecoder{}).DecodeHeader(message.Header.Get("Subject"))
				if err != nil {
					t.Fatal(err)
				}
				body := subject + "\n" + string(decoded)
				for _, part := range []string{"Main NAS", "árvíz", "nas-main", "192.0.2.10", "web-1", "stack: web", "service: app", oldSHA, newSHA, "[Docklight] Updates available on Main NAS"} {
					if !strings.Contains(body, part) {
						t.Fatalf("email missing %q: %s", part, body)
					}
				}
			})
		}
	}
}

func readPacket(r *bufio.Reader) (byte, []byte, error) {
	header, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	size, multiplier := 0, 1
	for range 4 {
		b, e := r.ReadByte()
		if e != nil {
			return 0, nil, e
		}
		size += int(b&127) * multiplier
		if b&128 == 0 {
			payload := make([]byte, size)
			_, e = io.ReadFull(r, payload)
			return header, payload, e
		}
		multiplier *= 128
	}
	return 0, nil, io.ErrUnexpectedEOF
}

func TestMQTTQoS1PayloadAcknowledgementAndTLS(t *testing.T) {
	for _, mode := range []string{"tcp", "tls", "reject", "noack", "untrusted-tls"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ca := ""
			scheme := "tcp"
			if mode == "tls" || mode == "untrusted-tls" {
				config, path := testTLS(t)
				ca = path
				scheme = "ssl"
				listener = tls.NewListener(listener, config)
				if mode == "untrusted-tls" {
					ca = ""
				}
			}
			payloads := make(chan []byte, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, e := listener.Accept()
				if e != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				reader := bufio.NewReader(conn)
				header, connect, e := readPacket(reader)
				if e != nil || header != 0x10 {
					return
				}
				if !strings.Contains(string(connect), "test-user") || !strings.Contains(string(connect), "test-password") {
					t.Error("MQTT credentials not sent in CONNECT")
				}
				code := byte(0)
				if mode == "reject" {
					code = 5
				}
				_, _ = conn.Write([]byte{0x20, 2, 0, code})
				if code != 0 {
					return
				}
				header, data, e := readPacket(reader)
				if e != nil {
					return
				}
				if header != 0x32 || len(data) < 4 {
					t.Errorf("expected non-retained QoS1 publish, got %x", header)
					return
				}
				topicLen := int(binary.BigEndian.Uint16(data[:2]))
				if topicLen+4 > len(data) {
					return
				}
				if string(data[2:2+topicLen]) != "docklight/updates" {
					t.Error("wrong MQTT topic")
				}
				packetID := data[2+topicLen : 4+topicLen]
				payloads <- data[4+topicLen:]
				if mode != "noack" {
					_, _ = conn.Write([]byte{0x40, 2, packetID[0], packetID[1]})
				}
				_, _, _ = readPacket(reader)
			}()
			c := MQTTConfig{Broker: scheme + "://" + listener.Addr().String(), User: "test-user", Password: "test-password", Topic: "docklight/updates", CAFile: ca}
			ctx := context.Background()
			if mode == "noack" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			err = c.Send(ctx, notificationEvent())
			if (err != nil) != (mode == "reject" || mode == "noack" || mode == "untrusted-tls") {
				t.Fatalf("MQTT %s: %v", mode, err)
			}
			<-done
			if mode == "reject" || mode == "untrusted-tls" {
				return
			}
			var event Event
			if err = json.Unmarshal(<-payloads, &event); err != nil {
				t.Fatal(err)
			}
			if event.Host.Name != "Main NAS" || event.Host.Hostname != "nas-main" || event.Host.Address != "https://192.0.2.10:8081" || event.Targets[0].Stack != "web" || event.Targets[0].Service != "app" || event.Deployed != oldSHA || event.Available != newSHA {
				t.Fatalf("incomplete MQTT payload %+v", event)
			}
		})
	}
}
