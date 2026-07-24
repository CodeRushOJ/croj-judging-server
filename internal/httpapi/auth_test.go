package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

type credentialStoreStub struct {
	credential *Credential
	err        error
	prefix     string
}

func (store *credentialStoreStub) FindCredentialByPrefix(_ context.Context, prefix string) (*Credential, error) {
	store.prefix = prefix
	return store.credential, store.err
}

func TestAuthenticatorAcceptsOnlyAValidActivePepperedOpaqueKey(t *testing.T) {
	pepper := []byte("0123456789abcdef0123456789abcdef")
	secret := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	key := "croj_public12_" + secret
	digest := hmac.New(sha256.New, pepper)
	_, _ = digest.Write([]byte(key))
	store := &credentialStoreStub{credential: &Credential{
		TenantID: "tenant-7",
		Digest:   digest.Sum(nil),
		Scopes:   []Scope{ScopeCapabilitiesRead, ScopeJobSubmit},
	}}
	authenticator, err := NewAuthenticator(store, pepper)
	if err != nil {
		t.Fatal(err)
	}
	authenticator.now = func() time.Time { return time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC) }

	principal, err := authenticator.Authenticate(context.Background(), "Bearer "+key)
	if err != nil {
		t.Fatal(err)
	}
	if principal.TenantID != "tenant-7" || !principal.Has(ScopeCapabilitiesRead) || !principal.Has(ScopeJobSubmit) {
		t.Fatalf("principal = %+v", principal)
	}
	if store.prefix != "public12" {
		t.Fatalf("lookup prefix = %q", store.prefix)
	}
}

func TestAuthenticatorAcceptsGeneratedBase64URLSecretContainingUnderscore(t *testing.T) {
	pepper := bytes.Repeat([]byte{0x41}, sha256.Size)
	secret := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, sha256.Size))
	key := "croj_public12_" + secret
	store := &credentialStoreStub{credential: &Credential{
		TenantID: "tenant-7", Digest: keyDigest(pepper, key), Scopes: []Scope{ScopeBundleWrite},
	}}
	authenticator, err := NewAuthenticator(store, pepper)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := authenticator.Authenticate(context.Background(), "Bearer "+key)
	if err != nil || principal.TenantID != "tenant-7" || !principal.Has(ScopeBundleWrite) {
		t.Fatalf("principal=%+v error=%v", principal, err)
	}
}

func TestAuthenticatorReturnsOneUniformErrorForInvalidCredentials(t *testing.T) {
	pepper := []byte("0123456789abcdef0123456789abcdef")
	validSecret := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	validKey := "croj_public12_" + validSecret
	now := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)

	tests := map[string]struct {
		header     string
		credential *Credential
		storeErr   error
	}{
		"missing":      {},
		"wrong scheme": {header: "Basic abc"},
		"malformed":    {header: "Bearer croj_short_not-base64!"},
		"unknown":      {header: "Bearer " + validKey},
		"wrong digest": {header: "Bearer " + validKey, credential: &Credential{TenantID: "tenant-7", Digest: make([]byte, sha256.Size)}},
		"revoked":      {header: "Bearer " + validKey, credential: &Credential{TenantID: "tenant-7", Digest: keyDigest(pepper, validKey), RevokedAt: pointer(now.Add(-time.Minute))}},
		"expired":      {header: "Bearer " + validKey, credential: &Credential{TenantID: "tenant-7", Digest: keyDigest(pepper, validKey), ExpiresAt: pointer(now.Add(-time.Second))}},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			store := &credentialStoreStub{credential: test.credential, err: test.storeErr}
			authenticator, err := NewAuthenticator(store, pepper)
			if err != nil {
				t.Fatal(err)
			}
			authenticator.now = func() time.Time { return now }
			if _, err := authenticator.Authenticate(context.Background(), test.header); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("error = %v, want uniform unauthenticated", err)
			}
		})
	}
}

func TestAuthenticatorPreservesCredentialRepositoryUnavailability(t *testing.T) {
	pepper := []byte("0123456789abcdef0123456789abcdef")
	secret := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	storeError := errors.New("database unavailable with internal address")
	authenticator, err := NewAuthenticator(&credentialStoreStub{err: storeError}, pepper)
	if err != nil {
		t.Fatal(err)
	}
	_, err = authenticator.Authenticate(context.Background(), "Bearer croj_public12_"+secret)
	if !errors.Is(err, ErrAuthenticationUnavailable) || !errors.Is(err, storeError) {
		t.Fatalf("error = %v, want typed unavailable preserving cause", err)
	}
}

func TestAuthenticatorAcceptsCaseInsensitiveBearerWithOneOrMoreSpaces(t *testing.T) {
	pepper := []byte("0123456789abcdef0123456789abcdef")
	secret := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	key := "croj_public12_" + secret
	store := &credentialStoreStub{credential: &Credential{
		TenantID: "tenant-7",
		Digest:   keyDigest(pepper, key),
		Scopes:   []Scope{ScopeCapabilitiesRead},
	}}
	authenticator, err := NewAuthenticator(store, pepper)
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"bearer " + key, "BEARER   " + key} {
		if _, err := authenticator.Authenticate(context.Background(), header); err != nil {
			t.Fatalf("header %q: %v", header, err)
		}
	}
}

func TestAuthenticatorRejectsUnsafeConfiguration(t *testing.T) {
	if _, err := NewAuthenticator(nil, make([]byte, 32)); err == nil {
		t.Fatal("expected nil store rejection")
	}
	if _, err := NewAuthenticator(&credentialStoreStub{}, []byte("too-short")); err == nil {
		t.Fatal("expected short pepper rejection")
	}
}

func keyDigest(pepper []byte, key string) []byte {
	digest := hmac.New(sha256.New, pepper)
	_, _ = digest.Write([]byte(key))
	return digest.Sum(nil)
}

func pointer(value time.Time) *time.Time { return &value }
