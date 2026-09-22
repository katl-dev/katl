package payloadbundle

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type httpLayout struct {
	base   *url.URL
	client *http.Client
}

func newHTTPLayout(base string, client *http.Client) (*httpLayout, error) {
	location, err := url.Parse(base)
	if err != nil || location.Host == "" || (location.Scheme != "http" && location.Scheme != "https") || location.User != nil || location.RawQuery != "" || location.Fragment != "" {
		return nil, fmt.Errorf("invalid HTTP OCI layout URL")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	return &httpLayout{
		base:   location,
		client: client,
	}, nil
}

func (layout *httpLayout) open(ctx context.Context, method, pin string) (*http.Response, error) {
	if !validDigest(pin) {
		return nil, fmt.Errorf("OCI layout requires a SHA-256 digest")
	}
	location := layout.base.JoinPath("blobs", "sha256", strings.TrimPrefix(pin, "sha256:"))
	request, err := http.NewRequestWithContext(ctx, method, location.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := layout.client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("read OCI layout blob %s: %s", pin, response.Status)
	}
	return response, nil
}

func (layout *httpLayout) Resolve(ctx context.Context, pin string) (ocispec.Descriptor, error) {
	response, err := layout.open(ctx, http.MethodHead, pin)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer response.Body.Close()
	if response.ContentLength <= 0 || response.ContentLength > 32<<20 {
		return ocispec.Descriptor{}, fmt.Errorf("OCI layout manifest requires a bounded Content-Length")
	}
	return ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.Digest(pin),
		Size:      response.ContentLength,
	}, nil
}

func (layout *httpLayout) Fetch(ctx context.Context, descriptor ocispec.Descriptor) (io.ReadCloser, error) {
	response, err := layout.open(ctx, http.MethodGet, descriptor.Digest.String())
	if err != nil {
		return nil, err
	}
	return response.Body, nil
}
