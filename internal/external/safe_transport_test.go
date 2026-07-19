package external

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"
)

type resolverStub struct {
	addresses []netip.Addr
	err       error
}

func (resolver resolverStub) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return resolver.addresses, resolver.err
}

func TestResolvePublicCallbackRejectsPrivateReservedAndMixedDNS(t *testing.T) {
	privateAddresses := []string{
		"0.0.0.0", "10.0.0.1", "100.64.0.1", "127.0.0.1", "169.254.169.254", "172.16.0.1", "192.168.1.1", "224.0.0.1",
		"::", "::1", "64:ff9b:1::1", "100:0:0:1::1", "2001::1", "2001:2::1", "2001:db8::1", "2002::1", "3fff::1",
		"4000::1", "fc00::1", "fec0::1", "fe80::1", "ff02::1", "::ffff:127.0.0.1",
	}
	for _, raw := range privateAddresses {
		t.Run(raw, func(t *testing.T) {
			_, err := resolvePublicCallback(context.Background(), resolverStub{addresses: []netip.Addr{netip.MustParseAddr(raw)}}, "hooks.example.com")
			if !errors.Is(err, ErrUnsafeCallbackDestination) {
				t.Fatalf("private or reserved callback address error=%v", err)
			}
		})
	}
	_, err := resolvePublicCallback(context.Background(), resolverStub{addresses: []netip.Addr{
		netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1"),
	}}, "hooks.example.com")
	if !errors.Is(err, ErrUnsafeCallbackDestination) {
		t.Fatalf("mixed public/private DNS answer error=%v", err)
	}
}

func TestSafeCallbackResolverOutageRemainsRetryable(t *testing.T) {
	want := errors.New("temporary resolver outage")
	_, err := resolvePublicCallback(context.Background(), resolverStub{err: want}, "hooks.example.com")
	if !errors.Is(err, want) || errors.Is(err, ErrUnsafeCallbackDestination) {
		t.Fatalf("resolver outage classification changed: %v", err)
	}
}

func TestSafeCallbackTransportBoundsUntrustedResponseHeadersAndConnections(t *testing.T) {
	client, err := NewSafeCallbackClient("hooks.example.com", 443, time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	wrapper, ok := client.Transport.(*safeCallbackTransport)
	if !ok || wrapper.transport.MaxResponseHeaderBytes != 64<<10 || wrapper.transport.MaxConnsPerHost <= 0 {
		t.Fatalf("transport=%T config=%+v", client.Transport, wrapper)
	}
}

func TestResolvePublicCallbackAcceptsOnlyGlobalUnicastAnswers(t *testing.T) {
	addresses, err := resolvePublicCallback(context.Background(), resolverStub{addresses: []netip.Addr{
		netip.MustParseAddr("2001:4860:4860::8888"), netip.MustParseAddr("8.8.8.8"),
	}}, "hooks.example.com")
	if err != nil || len(addresses) != 2 || addresses[0].String() != "8.8.8.8" {
		t.Fatalf("addresses=%v err=%v", addresses, err)
	}
}

func TestValidateCallbackDestinationEnforcesRegisteredHTTPSAuthority(t *testing.T) {
	for _, raw := range []string{
		"http://hooks.example.com/croj", "https://user@hooks.example.com/croj", "https://hooks.example.com:8443/croj",
		"https://127.0.0.1/croj", "https://[::1]/croj", "https://hooks.example.com/croj#fragment",
	} {
		if _, err := validateCallbackDestination(raw, "hooks.example.com", 443); err == nil {
			t.Fatalf("unsafe destination accepted: %s", raw)
		}
	}
	parsed, err := validateCallbackDestination("https://hooks.example.com/croj?tenant=42", "hooks.example.com", 443)
	if err != nil || parsed.Hostname() != "hooks.example.com" {
		t.Fatalf("destination=%v err=%v", parsed, err)
	}
}
