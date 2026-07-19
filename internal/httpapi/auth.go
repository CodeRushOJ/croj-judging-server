package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type Scope string

const (
	ScopeCapabilitiesRead Scope = "capabilities:read"
	ScopeBundleWrite      Scope = "bundle:write"
	ScopeBundleRead       Scope = "bundle:read"
	ScopeJobSubmit        Scope = "job:submit"
	ScopeJobRead          Scope = "job:read"
	ScopeJobCancel        Scope = "job:cancel"
)

var (
	ErrUnauthenticated = errors.New("request is not authenticated")
	keyPrefixPattern   = regexp.MustCompile(`^[A-Za-z0-9]{8,24}$`)
)

type Credential struct {
	TenantID  string
	Digest    []byte
	Scopes    []Scope
	ExpiresAt *time.Time
	RevokedAt *time.Time
}

type CredentialStore interface {
	FindCredentialByPrefix(context.Context, string) (*Credential, error)
}

type Principal struct {
	TenantID string
	scopes   map[Scope]struct{}
}

func (principal Principal) Has(scope Scope) bool {
	_, ok := principal.scopes[scope]
	return ok
}

type Authenticator struct {
	store  CredentialStore
	pepper []byte
	now    func() time.Time
}

func NewAuthenticator(store CredentialStore, pepper []byte) (*Authenticator, error) {
	if store == nil {
		return nil, fmt.Errorf("credential store is required")
	}
	if len(pepper) < sha256.Size {
		return nil, fmt.Errorf("credential pepper must contain at least 256 bits")
	}
	return &Authenticator{
		store:  store,
		pepper: append([]byte(nil), pepper...),
		now:    time.Now,
	}, nil
}

func (authenticator *Authenticator) Authenticate(ctx context.Context, authorization string) (Principal, error) {
	key, prefix, ok := parseBearerKey(authorization)
	if !ok {
		return Principal{}, ErrUnauthenticated
	}

	want := authenticator.digest(key)
	credential, err := authenticator.store.FindCredentialByPrefix(ctx, prefix)
	if err != nil || credential == nil {
		// Keep the unknown-prefix path on the same digest/constant-time comparison shape.
		_ = subtle.ConstantTimeCompare(want, make([]byte, sha256.Size))
		return Principal{}, ErrUnauthenticated
	}
	if len(credential.Digest) != sha256.Size || subtle.ConstantTimeCompare(want, credential.Digest) != 1 {
		return Principal{}, ErrUnauthenticated
	}
	now := authenticator.now()
	if credential.TenantID == "" || credential.RevokedAt != nil || (credential.ExpiresAt != nil && !now.Before(*credential.ExpiresAt)) {
		return Principal{}, ErrUnauthenticated
	}

	principal := Principal{TenantID: credential.TenantID, scopes: make(map[Scope]struct{}, len(credential.Scopes))}
	for _, scope := range credential.Scopes {
		principal.scopes[scope] = struct{}{}
	}
	return principal, nil
}

func (authenticator *Authenticator) digest(key string) []byte {
	digest := hmac.New(sha256.New, authenticator.pepper)
	_, _ = digest.Write([]byte(key))
	return digest.Sum(nil)
}

func parseBearerKey(authorization string) (key string, prefix string, ok bool) {
	if !strings.HasPrefix(authorization, "Bearer ") || strings.Count(authorization, " ") != 1 {
		return "", "", false
	}
	key = strings.TrimPrefix(authorization, "Bearer ")
	parts := strings.Split(key, "_")
	if len(parts) != 3 || parts[0] != "croj" || !keyPrefixPattern.MatchString(parts[1]) {
		return "", "", false
	}
	secret, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(secret) != sha256.Size {
		return "", "", false
	}
	return key, parts[1], true
}
