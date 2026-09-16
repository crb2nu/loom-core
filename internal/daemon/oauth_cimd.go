package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	cimdMaxDocumentSize = 64 << 10
	cimdFetchTimeout    = 5 * time.Second
	cimdCacheTTL        = 5 * time.Minute
)

type cimdMetadata struct {
	ClientID                      string   `json:"client_id"`
	RedirectURIs                  []string `json:"redirect_uris"`
	TokenEndpointAuthMethod       string   `json:"token_endpoint_auth_method"`
	GrantTypes                    []string `json:"grant_types"`
	ResponseTypes                 []string `json:"response_types"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
}

type cimdCacheEntry struct {
	metadata  *cimdMetadata
	expiresAt time.Time
}

type cimdResolver struct {
	client   *http.Client
	lookupIP func(context.Context, string) ([]net.IPAddr, error)
	now      func() time.Time
	ttl      time.Duration

	mu    sync.RWMutex
	cache map[string]cimdCacheEntry
}

func newCIMDResolver() *cimdResolver {
	r := &cimdResolver{
		lookupIP: net.DefaultResolver.LookupIPAddr,
		now:      time.Now,
		ttl:      cimdCacheTTL,
		cache:    make(map[string]cimdCacheEntry),
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = r.dialPublicContext
	r.client = &http.Client{
		Transport: transport,
		Timeout:   cimdFetchTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return r
}

func isCIMDClientID(clientID string) bool {
	u, err := url.Parse(clientID)
	return err == nil && strings.EqualFold(u.Scheme, "https") && u.Host != ""
}

func (r *cimdResolver) resolve(ctx context.Context, clientID string) (*cimdMetadata, error) {
	u, err := url.Parse(clientID)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("CIMD client_id must be an HTTPS URL without userinfo")
	}
	if u.Fragment != "" {
		return nil, errors.New("CIMD client_id must not contain a fragment")
	}

	now := r.now()
	r.mu.RLock()
	cached, ok := r.cache[clientID]
	r.mu.RUnlock()
	if ok && now.Before(cached.expiresAt) {
		return cached.metadata, nil
	}

	if err := r.validatePublicHost(ctx, u.Hostname()); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)
	if err != nil {
		return nil, fmt.Errorf("build CIMD request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch CIMD: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch CIMD: unexpected HTTP status %d", resp.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil, errors.New("CIMD response Content-Type must be application/json")
	}
	limited := io.LimitReader(resp.Body, cimdMaxDocumentSize+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read CIMD: %w", err)
	}
	if len(body) > cimdMaxDocumentSize {
		return nil, fmt.Errorf("CIMD document exceeds %d bytes", cimdMaxDocumentSize)
	}
	var metadata cimdMetadata
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(&metadata); err != nil {
		return nil, fmt.Errorf("decode CIMD: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("decode CIMD: document must contain one JSON object")
	}
	if err := validateCIMDMetadata(clientID, &metadata); err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.cache[clientID] = cimdCacheEntry{metadata: &metadata, expiresAt: now.Add(r.ttl)}
	r.mu.Unlock()
	return &metadata, nil
}

func (r *cimdResolver) validatePublicHost(ctx context.Context, hostname string) error {
	if hostname == "" || strings.EqualFold(hostname, "localhost") {
		return errors.New("CIMD host is not public")
	}
	addrs, err := r.lookupIP(ctx, hostname)
	if err != nil {
		return fmt.Errorf("resolve CIMD host: %w", err)
	}
	if len(addrs) == 0 {
		return errors.New("CIMD host did not resolve")
	}
	for _, addr := range addrs {
		if !isPublicIP(addr.IP) {
			return fmt.Errorf("CIMD host resolves to non-public address %s", addr.IP)
		}
	}
	return nil
}

func (r *cimdResolver) dialPublicContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("parse CIMD connection address: %w", err)
	}
	if err := r.validatePublicHost(ctx, host); err != nil {
		return nil, err
	}
	addrs, err := r.lookupIP(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve CIMD connection host: %w", err)
	}
	dialer := &net.Dialer{}
	var lastErr error
	for _, addr := range addrs {
		if !isPublicIP(addr.IP) {
			continue
		}
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(addr.IP.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	return nil, fmt.Errorf("connect to CIMD host: %w", lastErr)
}

func isPublicIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}
	// IsGlobalUnicast includes documentation, benchmarking, shared-address, and
	// other special-purpose ranges. None are valid CIMD origins, and some (most
	// notably carrier-grade NAT) can expose internal services in real networks.
	for _, prefix := range nonPublicCIMDPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

var nonPublicCIMDPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),   // shared address space
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
}

func validateCIMDMetadata(clientID string, m *cimdMetadata) error {
	if m.ClientID != clientID {
		return errors.New("CIMD client_id does not match document URL")
	}
	if len(m.RedirectURIs) == 0 {
		return errors.New("CIMD redirect_uris is required")
	}
	if m.TokenEndpointAuthMethod != "none" {
		return errors.New("CIMD token_endpoint_auth_method must be none")
	}
	if !containsString(m.GrantTypes, "authorization_code") || !containsString(m.ResponseTypes, "code") {
		return errors.New("CIMD must support authorization_code and code response type")
	}
	if !containsString(m.CodeChallengeMethodsSupported, "S256") {
		return errors.New("CIMD must support PKCE S256")
	}
	return nil
}
