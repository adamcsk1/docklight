package imageupdates

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type testKeychain struct{ auth authn.Authenticator }

func (k testKeychain) Resolve(authn.Resource) (authn.Authenticator, error) { return k.auth, nil }

func configImage(t *testing.T, architecture, variant, label string) v1.Image {
	t.Helper()
	image, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{OS: "linux", Architecture: architecture, Variant: variant, Config: v1.Config{Labels: map[string]string{"test": label}}})
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func TestRegistryPlatformSHAAndNoLayerPulls(t *testing.T) {
	var checking atomic.Bool
	var mutations atomic.Int32
	var authOK atomic.Bool
	var layerGets atomic.Int32
	layer := static.NewLayer([]byte("test layer content"), types.OCILayer)
	layerSHA, _ := layer.Digest()
	authOK.Store(true)
	backend := registry.New()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if checking.Load() && r.Method != http.MethodGet && r.Method != http.MethodHead {
			mutations.Add(1)
		}
		if checking.Load() && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, layerSHA.String()) {
			layerGets.Add(1)
		}
		user, password, _ := r.BasicAuth()
		if checking.Load() && !authOK.Load() && (user != "reader" || password != "test-password") {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(401)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		endpoint := *r.URL
		endpoint.Scheme = "https"
		endpoint.Host = u.Host
		copy.URL = &endpoint
		copy.Host = u.Host
		return server.Client().Transport.RoundTrip(copy)
	})
	ref, _ := name.NewTag("registry.example/test:latest")
	amd, err := mutate.AppendLayers(configImage(t, "amd64", "", "old"), layer)
	if err != nil {
		t.Fatal(err)
	}
	arm := configImage(t, "arm", "v7", "arm")
	index := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: amd, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}}, mutate.IndexAddendum{Add: arm, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm", Variant: "v7"}}})
	if err := remote.WriteIndex(ref, index, remote.WithTransport(transport)); err != nil {
		t.Fatal(err)
	}
	checking.Store(true)
	r := Registry{Transport: transport, Keychain: testKeychain{authn.Anonymous}}
	for _, tc := range []struct {
		arch, variant string
		image         v1.Image
	}{{"amd64", "", amd}, {"arm", "v7", arm}} {
		got, err := r.Resolve(context.Background(), ref.String(), "linux", tc.arch, tc.variant)
		want, _ := tc.image.ConfigName()
		if err != nil || got != want.String() {
			t.Fatalf("%s/%s got %s want %s: %v", tc.arch, tc.variant, got, want, err)
		}
	}
	if _, err := r.Resolve(context.Background(), ref.String(), "linux", "arm64", ""); err == nil {
		t.Fatal("missing platform accepted")
	}
	if mutations.Load() != 0 {
		t.Fatal("registry check mutated registry")
	}
	if layerGets.Load() != 0 {
		t.Fatal("image detection downloaded a layer")
	}
	checking.Store(false)
	if err := remote.Write(ref, amd, remote.WithTransport(transport)); err != nil {
		t.Fatal(err)
	}
	checking.Store(true)
	if _, err := r.Resolve(context.Background(), ref.String(), "linux", "arm64", ""); err == nil {
		t.Fatal("single-manifest platform mismatch accepted")
	}
	authOK.Store(false)
	if _, err := r.Resolve(context.Background(), ref.String(), "linux", "amd64", ""); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("authentication failure hidden: %v", err)
	}
	private := Registry{Transport: transport, Keychain: testKeychain{authn.FromConfig(authn.AuthConfig{Username: "reader", Password: "test-password"})}}
	if _, err := private.Resolve(context.Background(), ref.String(), "linux", "amd64", ""); err != nil {
		t.Fatalf("private registry credentials failed: %v", err)
	}
}

func TestRegistryResponseBoundAndRateLimit(t *testing.T) {
	secure := boundedTransport{roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("registry transport allowed plaintext redirect")
		return nil, nil
	})}
	if _, err := secure.RoundTrip(httptest.NewRequest("GET", "http://registry.example/v2/test/manifests/latest", nil)); err == nil {
		t.Fatal("registry HTTPS downgrade accepted")
	}
	body := &boundedBody{ReadCloser: io.NopCloser(strings.NewReader("12345")), remaining: 4}
	if _, err := io.ReadAll(body); err == nil {
		t.Fatal("oversized registry response accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r := Registry{Keychain: testKeychain{authn.Anonymous}, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("private upstream diagnostic")), Request: r}, nil
	})}
	if _, err := r.Resolve(ctx, "registry.example/test:latest", "linux", "amd64", ""); err == nil || strings.Contains(err.Error(), "private upstream") {
		t.Fatalf("unsafe rate-limit result %v", err)
	}
}

func TestRegistryInvalidReferencesAndCancellation(t *testing.T) {
	r := Registry{Keychain: testKeychain{authn.Anonymous}, Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid reference reached network")
		return nil, nil
	})}
	for _, image := range []string{"test@sha256:" + strings.Repeat("a", 64), "sha256:" + strings.Repeat("a", 64), "https://user:password@registry.example/image:tag", "localhost:5000/image:tag"} {
		if _, err := r.Resolve(context.Background(), image, "linux", "amd64", ""); err == nil {
			t.Fatalf("unsafe image %q", image)
		}
	}
	if _, err := r.Resolve(context.Background(), "registry.example/image:latest", "", "amd64", ""); err == nil {
		t.Fatal("missing platform accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (Registry{Keychain: testKeychain{authn.Anonymous}}).Resolve(ctx, "registry.example/image:latest", "linux", "amd64", "")
	if err == nil {
		t.Fatal("cancelled check succeeded")
	}
}

// Keep logical hosts distinct while using real, certificate-verified local TLS connections.
func registryFixtureTransport(t *testing.T, handlers map[string]http.Handler) http.RoundTripper {
	t.Helper()
	endpoints := map[string]*url.URL{}
	roots := x509.NewCertPool()
	for host, handler := range handlers {
		server := httptest.NewTLSServer(handler)
		t.Cleanup(server.Close)
		endpoint, _ := url.Parse(server.URL)
		endpoints[host] = endpoint
		roots.AddCert(server.Certificate())
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	t.Cleanup(transport.CloseIdleConnections)
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		endpoint, ok := endpoints[r.URL.Hostname()]
		if !ok {
			return nil, errors.New("unexpected registry fixture host")
		}
		copy := r.Clone(r.Context())
		address := *r.URL
		address.Host = endpoint.Host
		copy.URL = &address
		copy.Host = r.URL.Host
		response, err := transport.RoundTrip(copy)
		if response != nil {
			response.Request = r
		}
		return response, err
	})
}

func TestRegistryMissingVariantFailsClosed(t *testing.T) {
	backend := registry.New()
	transport := registryFixtureTransport(t, map[string]http.Handler{"registry.example": backend})
	ref, _ := name.NewTag("registry.example/test:latest")
	arm6, arm7 := configImage(t, "arm", "v6", "arm6"), configImage(t, "arm", "v7", "arm7")
	add := func(image v1.Image, variant string) mutate.IndexAddendum {
		return mutate.IndexAddendum{Add: image, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm", Variant: variant}}}
	}
	index := mutate.AppendManifests(empty.Index, add(arm6, "v6"), add(arm7, "v7"))
	r := Registry{Transport: transport, Keychain: testKeychain{authn.Anonymous}}
	for _, nested := range []bool{false, true} {
		published := index
		if nested {
			published = mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: index})
		}
		if err := remote.WriteIndex(ref, published, remote.WithTransport(transport)); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Resolve(context.Background(), ref.String(), "linux", "arm", ""); err == nil {
			t.Fatal("missing deployed variant selected first ARM image")
		}
		got, err := r.Resolve(context.Background(), ref.String(), "linux", "arm", "v7")
		want, _ := arm7.ConfigName()
		if err != nil || got != want.String() {
			t.Fatalf("explicit variant failed: %s %v", got, err)
		}
	}
	unique := mutate.AppendManifests(empty.Index, add(arm7, "v7"))
	if err := remote.WriteIndex(ref, unique, remote.WithTransport(transport)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(context.Background(), ref.String(), "linux", "arm", ""); err != nil {
		t.Fatalf("unique platform should remain supported: %v", err)
	}
	tooDeep := unique
	for range 8 {
		tooDeep = mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: tooDeep})
	}
	if err := remote.WriteIndex(ref, tooDeep, remote.WithTransport(transport)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(context.Background(), ref.String(), "linux", "arm", "v7"); err == nil || !strings.Contains(err.Error(), "nesting exceeds limit") {
		t.Fatalf("deep index nesting not bounded: %v", err)
	}
}

func TestRegistryBearerRedirectTrustBoundaries(t *testing.T) {
	for _, mode := range []string{"success", "cross-host-challenge", "manifest-downgrade", "token-downgrade", "auth-error", "cdn-error"} {
		t.Run(mode, func(t *testing.T) {
			const password = "fixture-password-secret"
			const token = "fixture-registry-token-secret"
			const cdnToken = "fixture-cdn-token-secret"
			const urlSecret = "fixture-url-secret"
			backend := registry.New()
			var checking atomic.Bool
			var authCalls, bearerCalls, tokenRedirects, cdnCalls, cdnAuthCalls, plaintextCalls atomic.Int32
			plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { plaintextCalls.Add(1); http.Error(w, password, 401) }))
			defer plain.Close()
			handlers := map[string]http.Handler{
				"registry.example": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !checking.Load() {
						backend.ServeHTTP(w, r)
						return
					}
					if r.Header.Get("Authorization") != "Bearer "+token {
						w.Header().Set("WWW-Authenticate", `Bearer realm="https://auth.example/token?private=`+urlSecret+`",service="registry.example"`)
						w.WriteHeader(401)
						return
					}
					bearerCalls.Add(1)
					if mode == "manifest-downgrade" && strings.Contains(r.URL.Path, "/manifests/") {
						http.Redirect(w, r, plain.URL+"/?private="+urlSecret, 302)
						return
					}
					if strings.Contains(r.URL.Path, "/blobs/") {
						http.Redirect(w, r, "https://cdn.example"+r.URL.Path+"?private="+urlSecret, 302)
						return
					}
					backend.ServeHTTP(w, r)
				}),
				"auth.example": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					authCalls.Add(1)
					user, secret, ok := r.BasicAuth()
					if !ok || user != "reader" || secret != password {
						t.Error("token exchange did not use registry credentials")
					}
					if r.URL.Query().Get("scope") != "repository:test:pull" {
						t.Error("incorrect bearer scope")
					}
					if mode == "auth-error" {
						http.Error(w, password+token+urlSecret, 401)
						return
					}
					if mode == "token-downgrade" {
						http.Redirect(w, r, plain.URL+"/?private="+urlSecret, 302)
						return
					}
					http.Redirect(w, r, "https://token.example/final?private="+urlSecret, 302)
				}),
				"token.example": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					tokenRedirects.Add(1)
					if r.Header.Get("Authorization") != "" {
						t.Error("registry credentials leaked through cross-host token redirect")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"token": token, "expires_in": 3600})
				}),
				"cdn.example": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					cdnCalls.Add(1)
					auth := r.Header.Get("Authorization")
					if auth != "" && auth != "Bearer "+cdnToken {
						t.Error("registry credentials or bearer token leaked to CDN")
					}
					if mode == "cdn-error" {
						http.Error(w, password+token+urlSecret, 403)
						return
					}
					if mode == "cross-host-challenge" && auth == "" {
						w.Header().Set("WWW-Authenticate", `Bearer realm="https://cdn-auth.example/token",scope="repository:test:pull"`)
						w.WriteHeader(401)
						return
					}
					backend.ServeHTTP(w, r)
				}),
				"cdn-auth.example": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					cdnAuthCalls.Add(1)
					if r.Header.Get("Authorization") != "" {
						t.Error("registry credentials leaked to redirected host token service")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"token": cdnToken, "expires_in": 3600})
				}),
			}
			tlsTransport := registryFixtureTransport(t, handlers)
			plainURL, _ := url.Parse(plain.URL)
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				// Let the fixture reach HTTP if production downgrade protection regresses.
				if r.URL.Host == plainURL.Host {
					return http.DefaultTransport.RoundTrip(r)
				}
				return tlsTransport.RoundTrip(r)
			})
			ref, _ := name.NewTag("registry.example/test:latest")
			image := configImage(t, "amd64", "", "redirect-test")
			if err := remote.Write(ref, image, remote.WithTransport(transport)); err != nil {
				t.Fatal(err)
			}
			checking.Store(true)
			r := Registry{Transport: transport, Keychain: testKeychain{authn.FromConfig(authn.AuthConfig{Username: "reader", Password: password})}}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			got, err := r.Resolve(ctx, ref.String(), "linux", "amd64", "")
			success := mode == "success" || mode == "cross-host-challenge"
			if (err == nil) != success {
				t.Fatalf("%s result: %v", mode, err)
			}
			if success {
				want, _ := image.ConfigName()
				if got != want.String() || bearerCalls.Load() == 0 || tokenRedirects.Load() == 0 || cdnCalls.Load() == 0 {
					t.Fatal("successful check bypassed bearer/redirect flow")
				}
			}
			if authCalls.Load() == 0 {
				t.Fatal("bearer exchange not exercised")
			}
			if mode == "cross-host-challenge" && cdnAuthCalls.Load() == 0 {
				t.Fatal("cross-host bearer challenge not exercised")
			}
			if plaintextCalls.Load() != 0 {
				t.Fatal("registry or token redirect reached plaintext endpoint")
			}
			for _, secret := range []string{password, token, cdnToken, urlSecret} {
				if strings.Contains(fmt.Sprint(err), secret) {
					t.Fatal("registry error exposed secret")
				}
			}
		})
	}
}
