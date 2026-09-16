package daemon

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCIMDResolverFetchAndCache(t *testing.T) {
	var requests atomic.Int32
	var clientID string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprintf(w, `{"client_id":%q,"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code"],"response_types":["code"],"code_challenge_methods_supported":["S256"]}`, clientID)
	}))
	defer server.Close()
	clientID = strings.Replace(server.URL, "127.0.0.1", "client.example", 1)

	r := testCIMDResolver(server)
	now := time.Now()
	r.now = func() time.Time { return now }
	for range 2 {
		metadata, err := r.resolve(context.Background(), clientID)
		if err != nil {
			t.Fatalf("resolve CIMD: %v", err)
		}
		if !containsString(metadata.RedirectURIs, "http://127.0.0.1/callback") {
			t.Fatalf("unexpected redirect URIs: %v", metadata.RedirectURIs)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1 cached fetch", requests.Load())
	}
	now = now.Add(cimdCacheTTL + time.Second)
	if _, err := r.resolve(context.Background(), clientID); err != nil {
		t.Fatalf("resolve expired CIMD: %v", err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want refetch after cache expiry", requests.Load())
	}
}

func TestCIMDResolverFetchPolicy(t *testing.T) {
	t.Run("HTTPS only", func(t *testing.T) {
		r := newCIMDResolver()
		if _, err := r.resolve(context.Background(), "http://example.com/client.json"); err == nil {
			t.Fatal("expected HTTP client ID rejection")
		}
	})

	for _, host := range []string{"localhost", "127.0.0.1", "10.1.2.3", "169.254.1.1", "[::1]", "[fc00::1]"} {
		t.Run("reject "+host, func(t *testing.T) {
			r := newCIMDResolver()
			if _, err := r.resolve(context.Background(), "https://"+host+"/client.json"); err == nil {
				t.Fatal("expected non-public host rejection")
			}
		})
	}

	t.Run("redirect", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://elsewhere.example/client.json")
			w.WriteHeader(http.StatusFound)
		}))
		defer server.Close()
		r := testCIMDResolver(server)
		clientID := strings.Replace(server.URL, "127.0.0.1", "client.example", 1)
		if _, err := r.resolve(context.Background(), clientID); err == nil || !strings.Contains(err.Error(), "status 302") {
			t.Fatalf("expected redirect rejection, got %v", err)
		}
	})

	tests := []struct {
		name        string
		contentType string
		body        string
		want        string
	}{
		{name: "content type", contentType: "text/plain", body: `{}`, want: "Content-Type"},
		{name: "size cap", contentType: "application/json", body: strings.Repeat("x", cimdMaxDocumentSize+1), want: "exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			r := testCIMDResolver(server)
			clientID := strings.Replace(server.URL, "127.0.0.1", "client.example", 1)
			if _, err := r.resolve(context.Background(), clientID); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %q error, got %v", tt.want, err)
			}
		})
	}

	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(100 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
		}))
		defer server.Close()
		r := testCIMDResolver(server)
		r.client.Timeout = 10 * time.Millisecond
		clientID := strings.Replace(server.URL, "127.0.0.1", "client.example", 1)
		if _, err := r.resolve(context.Background(), clientID); err == nil {
			t.Fatal("expected timeout")
		}
	})
}

func TestIsPublicIP(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.169.254",
		"192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1",
		"::1", "fc00::1", "fe80::1", "2001:db8::1",
	} {
		t.Run("reject "+raw, func(t *testing.T) {
			if isPublicIP(net.ParseIP(raw)) {
				t.Fatalf("isPublicIP(%q) = true", raw)
			}
		})
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		t.Run("accept "+raw, func(t *testing.T) {
			if !isPublicIP(net.ParseIP(raw)) {
				t.Fatalf("isPublicIP(%q) = false", raw)
			}
		})
	}
}

func TestValidateCIMDMetadata(t *testing.T) {
	valid := cimdMetadata{
		ClientID: "https://client.example/metadata", RedirectURIs: []string{"http://localhost/cb"},
		TokenEndpointAuthMethod: "none", GrantTypes: []string{"authorization_code"},
		ResponseTypes: []string{"code"}, CodeChallengeMethodsSupported: []string{"S256"},
	}
	if err := validateCIMDMetadata(valid.ClientID, &valid); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
	mutations := []struct {
		name string
		edit func(*cimdMetadata)
	}{
		{"client ID", func(m *cimdMetadata) { m.ClientID = "https://other.example" }},
		{"redirect URIs", func(m *cimdMetadata) { m.RedirectURIs = nil }},
		{"auth method", func(m *cimdMetadata) { m.TokenEndpointAuthMethod = "client_secret_basic" }},
		{"grant type", func(m *cimdMetadata) { m.GrantTypes = nil }},
		{"response type", func(m *cimdMetadata) { m.ResponseTypes = nil }},
		{"PKCE", func(m *cimdMetadata) { m.CodeChallengeMethodsSupported = nil }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			got := valid
			mutation.edit(&got)
			if err := validateCIMDMetadata(valid.ClientID, &got); err == nil {
				t.Fatal("expected invalid metadata")
			}
		})
	}
}

func testCIMDResolver(server *httptest.Server) *cimdResolver {
	client := server.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	transport := client.Transport.(*http.Transport)
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- test server certificate
	serverAddress := server.Listener.Addr().String()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, serverAddress)
	}
	return &cimdResolver{
		client: client,
		lookupIP: func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		},
		now: time.Now, ttl: cimdCacheTTL, cache: make(map[string]cimdCacheEntry),
	}
}
