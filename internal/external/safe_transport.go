package external

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type callbackResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

var prohibitedCallbackPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"), netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("ff00::/8"),
}

func resolvePublicCallback(ctx context.Context, resolver callbackResolver, host string) ([]netip.Addr, error) {
	if resolver == nil || !validDNSName(host) {
		return nil, fmt.Errorf("callback resolver or host is invalid")
	}
	addresses, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve callback host: %w", err)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("callback host has no addresses")
	}
	result := make([]netip.Addr, 0, len(addresses))
	seen := make(map[netip.Addr]struct{}, len(addresses))
	for _, raw := range addresses {
		address := raw.Unmap()
		if !isPublicCallbackAddress(address) {
			return nil, fmt.Errorf("callback host resolved to a prohibited address")
		}
		if _, exists := seen[address]; !exists {
			seen[address] = struct{}{}
			result = append(result, address)
		}
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Compare(result[right]) < 0 })
	return result, nil
}

func isPublicCallbackAddress(address netip.Addr) bool {
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() ||
		address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() ||
		address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	for _, prefix := range prohibitedCallbackPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func parseHTTPSDestination(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" {
		return nil, fmt.Errorf("callback destination must be an absolute HTTPS URL without userinfo or fragment")
	}
	host := strings.ToLower(parsed.Hostname())
	if !validDNSName(host) {
		return nil, fmt.Errorf("callback destination host must be an ASCII DNS name")
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return nil, fmt.Errorf("callback destination must not use an IP literal")
	}
	if parsed.Port() != "" {
		port, err := strconv.ParseUint(parsed.Port(), 10, 16)
		if err != nil || port == 0 {
			return nil, fmt.Errorf("callback destination port is invalid")
		}
	}
	return parsed, nil
}

func validateCallbackDestination(raw, allowedHost string, allowedPort uint16) (*url.URL, error) {
	parsed, err := parseHTTPSDestination(raw)
	if err != nil {
		return nil, err
	}
	normalizedHost := strings.ToLower(strings.TrimSuffix(allowedHost, "."))
	if !validDNSName(normalizedHost) || strings.ToLower(strings.TrimSuffix(parsed.Hostname(), ".")) != normalizedHost {
		return nil, fmt.Errorf("callback destination does not match the registered host")
	}
	port := uint64(443)
	if parsed.Port() != "" {
		port, _ = strconv.ParseUint(parsed.Port(), 10, 16)
	}
	if allowedPort == 0 || port != uint64(allowedPort) {
		return nil, fmt.Errorf("callback destination does not match the registered port")
	}
	return parsed, nil
}

func validDNSName(host string) bool {
	if host == "" || len(host) > 253 || host != strings.ToLower(host) || strings.HasSuffix(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	return true
}

type safeCallbackTransport struct {
	host      string
	port      uint16
	resolver  callbackResolver
	transport *http.Transport
}

func NewSafeCallbackClient(host string, port uint16, requestTimeout, dialTimeout time.Duration) (*http.Client, error) {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if !validDNSName(host) || port == 0 || requestTimeout <= 0 || dialTimeout <= 0 {
		return nil, fmt.Errorf("safe callback client configuration is invalid")
	}
	resolver := net.DefaultResolver
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	transport := &safeCallbackTransport{host: host, port: port, resolver: resolver}
	transport.transport = &http.Transport{
		Proxy:               nil,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			addresses, err := resolvePublicCallback(ctx, resolver, host)
			if err != nil {
				return nil, err
			}
			var lastErr error
			for _, address := range addresses {
				connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), strconv.Itoa(int(port))))
				if err == nil {
					return connection, nil
				}
				lastErr = err
			}
			return nil, fmt.Errorf("dial callback host: %w", lastErr)
		},
	}
	return &http.Client{
		Timeout:       requestTimeout,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

func NewSafeWebhookDeliverer(host string, port uint16, requestTimeout, dialTimeout time.Duration) (*WebhookDeliverer, error) {
	client, err := NewSafeCallbackClient(host, port, requestTimeout, dialTimeout)
	if err != nil {
		return nil, err
	}
	return NewWebhookDeliverer(client.Transport, requestTimeout)
}

func (transport *safeCallbackTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport == nil || transport.transport == nil {
		return nil, fmt.Errorf("safe callback transport is not configured")
	}
	if _, err := validateCallbackDestination(request.URL.String(), transport.host, transport.port); err != nil {
		return nil, err
	}
	return transport.transport.RoundTrip(request)
}
