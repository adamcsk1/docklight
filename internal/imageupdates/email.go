package imageupdates

import (
	"bytes"
	"cmp"
	_ "embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"slices"
	"strings"
	texttemplate "text/template"
	"time"

	"docklight/assets"
	"github.com/google/uuid"
)

//go:embed email.html
var emailHTML string

//go:embed email.txt
var emailText string

var emailFuncs = template.FuncMap{
	"join":  strings.Join,
	"utc":   func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"count": countLabel,
}
var htmlEmail = template.Must(template.New("email").Funcs(emailFuncs).Parse(emailHTML))
var textEmail = texttemplate.Must(texttemplate.New("email").Funcs(texttemplate.FuncMap(emailFuncs)).Parse(emailText))

type digestUpdate struct {
	Event
	Stack, Service string
	Containers     []string
}

type digestStack struct {
	Name    string
	Updates []digestUpdate
}

type digestHost struct {
	HostIdentity
	Stacks []digestStack
}

type digest struct {
	Hosts                                 []digestHost
	Updates, Stacks, Services, Containers int
}

func countLabel(n int, noun string) string {
	if n != 1 {
		noun += "s"
	}
	return fmt.Sprintf("%d %s", n, noun)
}

func makeDigest(events []Event) digest {
	type key struct{ host, stack, service, event string }
	type serviceKey struct{ host, stack, service, container string }
	groups := map[key]digestUpdate{}
	services := map[serviceKey]bool{}
	containers := map[[2]string]bool{}
	for _, event := range events {
		for _, target := range event.Targets {
			k := key{event.Host.ID, target.Stack, target.Service, event.ID}
			u := groups[k]
			u.Event, u.Stack, u.Service = event, target.Stack, target.Service
			u.Containers = append(u.Containers, target.Container)
			groups[k] = u
			// Standalone containers have no Compose service; count them individually.
			service := serviceKey{host: event.Host.ID, stack: target.Stack, service: target.Service}
			if target.Stack == "" || target.Service == "" {
				service.service, service.container = "", target.Container
			}
			services[service] = true
			containers[[2]string{event.Host.ID, target.Container}] = true
		}
	}
	var rows []digestUpdate
	for _, row := range groups {
		slices.Sort(row.Containers)
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b digestUpdate) int {
		return cmp.Or(strings.Compare(a.Host.Name, b.Host.Name), strings.Compare(a.Host.ID, b.Host.ID), strings.Compare(a.Stack, b.Stack), strings.Compare(a.Service, b.Service), strings.Compare(a.Image, b.Image), strings.Compare(a.ID, b.ID))
	})
	d := digest{Updates: len(events), Services: len(services), Containers: len(containers)}
	for _, row := range rows {
		if len(d.Hosts) == 0 || d.Hosts[len(d.Hosts)-1].ID != row.Host.ID {
			d.Hosts = append(d.Hosts, digestHost{HostIdentity: row.Host})
		}
		host := &d.Hosts[len(d.Hosts)-1]
		if len(host.Stacks) == 0 || host.Stacks[len(host.Stacks)-1].Name != row.Stack {
			host.Stacks = append(host.Stacks, digestStack{Name: row.Stack})
			d.Stacks++
		}
		stack := &host.Stacks[len(host.Stacks)-1]
		stack.Updates = append(stack.Updates, row)
	}
	return d
}

func (c SMTPConfig) message(events []Event) ([]byte, error) {
	d := makeDigest(events)
	var body bytes.Buffer
	related := multipart.NewWriter(&body)
	var alternatives bytes.Buffer
	alternative := multipart.NewWriter(&alternatives)
	for _, part := range []struct {
		contentType string
		render      func(*bytes.Buffer) error
	}{
		{"text/plain", func(b *bytes.Buffer) error { return textEmail.Execute(b, d) }},
		{"text/html", func(b *bytes.Buffer) error { return htmlEmail.Execute(b, d) }},
	} {
		var rendered bytes.Buffer
		if err := part.render(&rendered); err != nil {
			return nil, err
		}
		w, err := alternative.CreatePart(textproto.MIMEHeader{"Content-Type": {part.contentType + "; charset=utf-8"}, "Content-Transfer-Encoding": {"quoted-printable"}})
		if err != nil {
			return nil, err
		}
		encoder := quotedprintable.NewWriter(w)
		if _, err = encoder.Write(rendered.Bytes()); err != nil {
			return nil, err
		}
		if err = encoder.Close(); err != nil {
			return nil, err
		}
	}
	if err := alternative.Close(); err != nil {
		return nil, err
	}
	w, err := related.CreatePart(textproto.MIMEHeader{"Content-Type": {`multipart/alternative; boundary="` + alternative.Boundary() + `"`}})
	if err != nil {
		return nil, err
	}
	if _, err = w.Write(alternatives.Bytes()); err != nil {
		return nil, err
	}
	w, err = related.CreatePart(textproto.MIMEHeader{"Content-Type": {"image/png"}, "Content-Transfer-Encoding": {"base64"}, "Content-ID": {"<docklight-logo>"}, "Content-Disposition": {`inline; filename="docklight.png"`}})
	if err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(assets.LogoPNG)
	for len(encoded) > 0 {
		n := min(76, len(encoded))
		if _, err = fmt.Fprint(w, encoded[:n]+"\r\n"); err != nil {
			return nil, err
		}
		encoded = encoded[n:]
	}
	if err = related.Close(); err != nil {
		return nil, err
	}
	var message bytes.Buffer
	fmt.Fprintf(&message, "From: %s\r\nTo: %s\r\nSubject: [Docklight] %s across %s\r\nDate: %s\r\nMessage-ID: <%s@docklight>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/related;\r\n type=\"multipart/alternative\";\r\n boundary=\"%s\"\r\n\r\n", c.From, strings.Join(c.To, ", "), countLabel(d.Updates, "image update"), countLabel(len(d.Hosts), "host"), time.Now().UTC().Format(time.RFC1123Z), uuid.NewString(), related.Boundary())
	message.Write(body.Bytes())
	return message.Bytes(), nil
}
