package imageupdates

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"docklight/assets"
)

func decodeEmail(t *testing.T, message *mail.Message) (plain, html string) {
	t.Helper()
	media, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil || media != "multipart/related" || params["type"] != "multipart/alternative" {
		t.Fatalf("invalid related message: %s %v", media, err)
	}
	related := multipart.NewReader(message.Body, params["boundary"])
	alternative, err := related.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	media, params, err = mime.ParseMediaType(alternative.Header.Get("Content-Type"))
	if err != nil || media != "multipart/alternative" {
		t.Fatalf("missing alternative: %s %v", media, err)
	}
	parts := multipart.NewReader(alternative, params["boundary"])
	for _, kind := range []string{"text/plain", "text/html"} {
		part, err := parts.NextPart() // Automatically decodes quoted-printable.
		if err != nil {
			t.Fatal(err)
		}
		media, params, err = mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil || media != kind || params["charset"] != "utf-8" {
			t.Fatalf("invalid text part %s %v %v", media, params, err)
		}
		body, err := io.ReadAll(part)
		if err != nil {
			t.Fatal(err)
		}
		if kind == "text/plain" {
			plain = string(body)
		} else {
			html = string(body)
		}
	}
	if _, err := parts.NextPart(); err != io.EOF {
		t.Fatal("extra alternative part", err)
	}
	logo, err := related.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if logo.Header.Get("Content-ID") != "<docklight-logo>" || logo.Header.Get("Content-Type") != "image/png" || !strings.HasPrefix(logo.Header.Get("Content-Disposition"), "inline;") || logo.Header.Get("Content-Transfer-Encoding") != "base64" {
		t.Fatal("invalid inline logo headers")
	}
	decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, logo))
	if err != nil || !bytes.Equal(decoded, assets.LogoPNG) {
		t.Fatal("embedded logo differs from Docklight branding", err)
	}
	if !strings.Contains(html, `src="cid:docklight-logo"`) || !strings.Contains(html, `alt="Docklight logo"`) {
		t.Fatal("HTML does not reference inline logo accessibly")
	}
	if _, err := related.NextPart(); err != io.EOF {
		t.Fatal("extra related part", err)
	}
	return plain, html
}

func TestEmailDigestGroupingEscapingAndMIME(t *testing.T) {
	first := notificationEvent()
	first.Host.Name = "A <script>alert(1)</script> & árvíz\r\nBcc: injected@example.com"
	first.Targets = append(first.Targets,
		Target{Container: "web-2", Stack: "web", Service: "app"},
		Target{Container: "worker", Stack: "web", Service: "worker"},
		Target{Container: "proxy", Stack: "edge", Service: "proxy"},
	)
	second := notificationEvent()
	second.ID, second.Image = "second", "registry.example/other:latest"
	second.Host.ID, second.Host.Name = "other", "Z host"
	second.Targets = []Target{{Container: "solo"}}
	events := []Event{second, first}
	d := makeDigest(events)
	if d.Updates != 2 || d.Stacks != 3 || d.Services != 4 || d.Containers != 5 || len(d.Hosts) != 2 || d.Hosts[0].ID != first.Host.ID || len(d.Hosts[0].Stacks) != 2 || d.Hosts[0].Stacks[0].Name != "edge" {
		t.Fatalf("bad summary/grouping: %+v", d)
	}
	if got := d.Hosts[0].Stacks[1].Updates; len(got) != 2 || got[0].Service != "app" || !slices.Equal(got[0].Containers, []string{"web-1", "web-2"}) {
		t.Fatalf("replicas not grouped by service: %+v", got)
	}
	c := SMTPConfig{From: "sender@example.com", To: []string{"to@example.com"}}
	wire, err := c.message(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(wire), "\r\n") {
		if len(line) > 998 {
			t.Fatal("SMTP line exceeds RFC limit")
		}
		for _, r := range line {
			if r > 127 {
				t.Fatal("non-ASCII wire data without 8BITMIME")
			}
		}
	}
	message, err := mail.ReadMessage(bytes.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	if message.Header.Get("Bcc") != "" || message.Header.Get("Subject") != "[Docklight] 2 image updates across 2 hosts" {
		t.Fatal("unsafe or incorrect headers")
	}
	plain, html := decodeEmail(t, message)
	for _, want := range []string{"2 image updates", "4 services", "5 containers", "Stack: web", "Stack: edge", "Standalone containers", "web-1, web-2", "Service: worker", "Z host", oldSHA, newSHA, "No update was installed"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("plain text missing %q", want)
		}
	}
	if strings.Contains(html, "<script>") || !strings.Contains(html, "&lt;script&gt;") || strings.Contains(html, `src="http`) || strings.Contains(html, "ZgotmplZ") {
		t.Fatal("unsafe HTML or external logo fetch")
	}
	// Rendering order must not depend on source or target iteration order.
	slices.Reverse(events)
	slices.Reverse(events[0].Targets)
	var rerendered bytes.Buffer
	// MIME readers normalize CRLF; compare normalized template output.
	if err := htmlEmail.Execute(&rerendered, makeDigest(events)); err != nil || strings.ReplaceAll(rerendered.String(), "\r\n", "\n") != strings.ReplaceAll(html, "\r\n", "\n") {
		t.Fatal("unstable digest ordering", err)
	}
}

func TestSMTPEmptyDigestDoesNotConnect(t *testing.T) {
	if err := (SMTPConfig{Host: "invalid.invalid"}).Send(context.Background(), nil); err != nil {
		t.Fatal("empty digest attempted SMTP", err)
	}
}

func TestEmailDigestServiceCountsWithPartialMetadata(t *testing.T) {
	e := notificationEvent()
	e.Targets = []Target{
		{Container: "app-1", Stack: "web", Service: "app"},
		{Container: "app-2", Stack: "web", Service: "app"},
		{Container: "app", Stack: "web"},
		{Container: "standalone-1"},
		{Container: "standalone-2"},
	}
	d := makeDigest([]Event{e})
	if d.Services != 4 || d.Containers != 5 || d.Stacks != 2 {
		t.Fatalf("partial metadata conflated services and container names: %+v", d)
	}
	if countLabel(1, "image update") != "1 image update" || countLabel(2, "host") != "2 hosts" {
		t.Fatal("incorrect summary grammar")
	}
}

func TestBrowserEmailDigest(t *testing.T) {
	if os.Getenv("DOCKLIGHT_UI_BROWSER_TEST") != "1" {
		t.Skip("set DOCKLIGHT_UI_BROWSER_TEST=1 with Playwright installed")
	}
	first := notificationEvent()
	first.Host.Name = "Main NAS <home>"
	first.Host.Address = "https://" + strings.Repeat("long-hostname", 20) + ".example:8081"
	first.Image = "registry.example/" + strings.Repeat("longimage", 20) + ":latest"
	first.Targets = append(first.Targets, Target{Container: "web-2", Stack: "web", Service: "app"}, Target{Container: "worker", Stack: "workers", Service: "worker"})
	second := notificationEvent()
	second.ID, second.Host.ID, second.Host.Name = "second", "other", "Other host"
	second.Targets = []Target{{Container: "standalone"}}
	var html bytes.Buffer
	if err := htmlEmail.Execute(&html, makeDigest([]Event{first, second})); err != nil {
		t.Fatal(err)
	}
	// Browsers do not resolve email Content-IDs; serve exactly the same logo locally.
	preview := strings.ReplaceAll(html.String(), "cid:docklight-logo", "/logo.png")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/logo.png" {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(assets.LogoPNG)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, preview)
	}))
	defer server.Close()
	output, err := exec.Command("node", "../../scripts/email-digest-smoke.cjs", server.URL).CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatal(err)
	}
}
