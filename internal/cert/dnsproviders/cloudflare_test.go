package dnsproviders

import (
	"io"
	"net/http"
	"strings"
	"testing"

	libcloudflare "github.com/libdns/cloudflare"
)

type captureHTTPClient struct {
	request *http.Request
}

func (c *captureHTTPClient) Do(req *http.Request) (*http.Response, error) {
	c.request = req
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
}

func TestCloudflareGlobalKeyAuthentication(t *testing.T) {
	entry, ok := Get("cloudflare")
	if !ok {
		t.Fatal("cloudflare provider is not registered")
	}
	built, err := entry.Build(map[string]string{
		"CLOUDFLARE_EMAIL":   "owner@example.com",
		"CLOUDFLARE_API_KEY": "global-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := built.(*libcloudflare.Provider)
	if !ok {
		t.Fatalf("unexpected provider type %T", built)
	}
	globalClient, ok := provider.HTTPClient.(*cloudflareGlobalKeyClient)
	if !ok {
		t.Fatalf("global-key client not installed: %T", provider.HTTPClient)
	}
	capture := &captureHTTPClient{}
	globalClient.client = capture
	req, _ := http.NewRequest(http.MethodGet, "https://api.cloudflare.com/client/v4/zones", nil)
	req.Header.Set("Authorization", "Bearer global-key")
	if _, err := globalClient.Do(req); err != nil {
		t.Fatal(err)
	}
	if capture.request.Header.Get("Authorization") != "" || capture.request.Header.Get("X-Auth-Email") != "owner@example.com" || capture.request.Header.Get("X-Auth-Key") != "global-key" {
		t.Fatalf("incorrect Cloudflare auth headers: %#v", capture.request.Header)
	}
}

func TestCloudflareCredentialsMustBeComplete(t *testing.T) {
	entry, _ := Get("cloudflare")
	if _, err := entry.Build(map[string]string{"CLOUDFLARE_API_KEY": "key-only"}); err == nil {
		t.Fatal("global API key without email was accepted")
	}
	if _, err := entry.Build(map[string]string{"CLOUDFLARE_DNS_API_TOKEN": "token"}); err != nil {
		t.Fatalf("API token was rejected: %v", err)
	}
}
