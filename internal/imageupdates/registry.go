package imageupdates

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

var imageSHA = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type RegistryHTTPError struct{ Status int }

func (e RegistryHTTPError) Error() string {
	return fmt.Sprintf("Registry check failed (HTTP %d)", e.Status)
}

func registryDomain(image string) string {
	ref, err := name.NewTag(image, name.WeakValidation)
	if err != nil {
		return ""
	}
	return ref.Context().RegistryStr()
}

type Registry struct {
	Transport http.RoundTripper
	Keychain  authn.Keychain
}

type boundedTransport struct{ http.RoundTripper }

func (t boundedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" {
		return nil, errors.New("registry metadata requires HTTPS, including redirects")
	}
	response, err := t.RoundTripper.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	response.Body = &boundedBody{ReadCloser: response.Body, remaining: 4 << 20}
	return response, nil
}

type boundedBody struct {
	io.ReadCloser
	remaining int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		var extra [1]byte
		n, err := b.ReadCloser.Read(extra[:])
		if n > 0 {
			return 0, errors.New("registry metadata exceeds size limit")
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, err
}

// Resolve fetches the config digest used as the image ID by Docker's classic store.
func (r Registry) Resolve(ctx context.Context, image, os, architecture, variant string) (string, error) {
	return r.resolve(ctx, image, os, architecture, variant, false)
}

// ResolveManifest fetches the selected platform manifest digest used by containerd containers.
func (r Registry) ResolveManifest(ctx context.Context, image, os, architecture, variant string) (string, error) {
	return r.resolve(ctx, image, os, architecture, variant, true)
}

func (r Registry) resolve(ctx context.Context, image, os, architecture, variant string, manifestDigest bool) (string, error) {
	if os == "" || architecture == "" || strings.Contains(image, "@") || strings.HasPrefix(image, "sha256:") {
		return "", errors.New("Image tag or deployed platform unavailable")
	}
	ref, err := name.NewTag(image, name.WeakValidation)
	if err != nil || ref.Context().Registry.Scheme() != "https" {
		return "", errors.New("Invalid image reference or insecure registry")
	}
	keychain := r.Keychain
	if keychain == nil {
		keychain = authn.DefaultKeychain
	}
	opts := []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(keychain), remote.WithPlatform(v1.Platform{OS: os, Architecture: architecture, Variant: variant}), remote.WithUserAgent("docklight-image-updates")}
	transport := r.Transport
	if transport == nil {
		transport = remote.DefaultTransport
	}
	opts = append(opts, remote.WithTransport(boundedTransport{transport}))
	descriptor, err := remote.Get(ref, opts...)
	if err != nil {
		return "", registryError(err)
	}
	// Do not let the library pick the first variant when deployed metadata is incomplete.
	for depth := 0; descriptor.MediaType.IsIndex(); depth++ {
		if depth == 8 {
			return "", errors.New("Registry image index nesting exceeds limit")
		}
		index, err := descriptor.ImageIndex()
		if err != nil {
			return "", registryError(err)
		}
		manifest, err := index.IndexManifest()
		if err != nil {
			return "", registryError(err)
		}
		var matches []v1.Descriptor
		for _, child := range manifest.Manifests {
			p := child.Platform
			if p == nil {
				if child.MediaType.IsIndex() {
					matches = append(matches, child)
				}
				continue
			}
			if p.OS == os && p.Architecture == architecture && (variant == "" || p.Variant == variant) {
				matches = append(matches, child)
			}
		}
		if len(matches) == 0 {
			return "", errors.New("Registry image platform does not match deployed image")
		}
		if len(matches) > 1 && variant == "" {
			return "", errors.New("Deployed image variant unavailable; registry platform is ambiguous")
		}
		descriptor, err = remote.Get(ref.Context().Digest(matches[0].Digest.String()), opts...)
		if err != nil {
			return "", registryError(err)
		}
	}
	img, err := descriptor.Image()
	if err != nil {
		return "", registryError(err)
	}
	config, err := img.ConfigName()
	if err != nil || !imageSHA.MatchString(config.String()) {
		return "", errors.New("Invalid registry image configuration digest")
	}
	manifest, err := img.Manifest()
	if err != nil || manifest.Config.Size < 0 || manifest.Config.Size > 1<<20 {
		return "", errors.New("Registry image configuration exceeds size limit or is invalid")
	}
	// A single-platform manifest also needs validation; WithPlatform selects index entries only.
	// ConfigFile downloads only the small configuration blob, never image layers.
	file, err := img.ConfigFile()
	if err != nil {
		return "", registryError(err)
	}
	if file.OS != os || file.Architecture != architecture || variant != "" && file.Variant != "" && file.Variant != variant {
		return "", errors.New("Registry image platform does not match deployed image")
	}
	if manifestDigest {
		if !imageSHA.MatchString(descriptor.Digest.String()) {
			return "", errors.New("Invalid registry manifest digest")
		}
		return descriptor.Digest.String(), nil
	}
	return config.String(), nil
}

func registryError(err error) error {
	var e *transport.Error
	if errors.As(err, &e) {
		return RegistryHTTPError{Status: e.StatusCode}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return errors.New("Registry check timed out or cancelled")
	}
	// Registry errors can contain tokens, URLs and arbitrary upstream response bodies.
	return errors.New("Registry check failed; verify connectivity, credentials and platform")
}
