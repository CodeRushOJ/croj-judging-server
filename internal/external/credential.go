package external

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
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

var validScopes = map[Scope]struct{}{
	ScopeCapabilitiesRead: {},
	ScopeBundleWrite:      {},
	ScopeBundleRead:       {},
	ScopeJobSubmit:        {},
	ScopeJobRead:          {},
	ScopeJobCancel:        {},
}

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

type credentialQueryer interface {
	QueryRowContext(context.Context, string, ...any) rowScanner
}

type sqlCredentialQueryer struct{ database *sql.DB }

func (queryer sqlCredentialQueryer) QueryRowContext(ctx context.Context, query string, arguments ...any) rowScanner {
	return queryer.database.QueryRowContext(ctx, query, arguments...)
}

type SQLCredentialStore struct{ queryer credentialQueryer }

func NewSQLCredentialStore(database *sql.DB) (*SQLCredentialStore, error) {
	if database == nil {
		return nil, fmt.Errorf("credential database is required")
	}
	return &SQLCredentialStore{queryer: sqlCredentialQueryer{database: database}}, nil
}

func (store *SQLCredentialStore) FindCredentialByPrefix(ctx context.Context, prefix string) (*Credential, error) {
	if store == nil || store.queryer == nil {
		return nil, fmt.Errorf("credential store is not configured")
	}
	var tenantID string
	var digest []byte
	var encodedScopes []byte
	var expiresAt sql.NullTime
	var revokedAt sql.NullTime
	err := store.queryer.QueryRowContext(ctx, `
SELECT tenant.external_id, api_key.key_digest, api_key.scopes_json,
       api_key.expires_at, api_key.revoked_at
FROM t_external_api_key AS api_key
JOIN t_external_tenant AS tenant ON tenant.id = api_key.tenant_id
WHERE api_key.lookup_prefix = ? AND tenant.status = 'ACTIVE'
LIMIT 1`, prefix).Scan(&tenantID, &digest, &encodedScopes, &expiresAt, &revokedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find credential: %w", err)
	}
	if tenantID == "" || len(digest) != sha256.Size {
		return nil, fmt.Errorf("stored credential is invalid")
	}
	scopes, err := decodeScopes(encodedScopes)
	if err != nil {
		return nil, fmt.Errorf("stored credential scopes: %w", err)
	}
	credential := &Credential{
		TenantID: tenantID,
		Digest:   append([]byte(nil), digest...),
		Scopes:   scopes,
	}
	if expiresAt.Valid {
		value := expiresAt.Time
		credential.ExpiresAt = &value
	}
	if revokedAt.Valid {
		value := revokedAt.Time
		credential.RevokedAt = &value
	}
	return credential, nil
}

func decodeScopes(encoded []byte) ([]Scope, error) {
	var scopes []Scope
	if err := json.Unmarshal(encoded, &scopes); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	if len(scopes) == 0 {
		return nil, fmt.Errorf("at least one scope is required")
	}
	seen := make(map[Scope]struct{}, len(scopes))
	for _, scope := range scopes {
		if _, valid := validScopes[scope]; !valid {
			return nil, fmt.Errorf("unknown scope %q", scope)
		}
		if _, duplicate := seen[scope]; duplicate {
			return nil, fmt.Errorf("duplicate scope %q", scope)
		}
		seen[scope] = struct{}{}
	}
	return scopes, nil
}
