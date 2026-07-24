package external

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSQLCredentialStoreLoadsOnlyAnActiveTenantAndStrictScopes(t *testing.T) {
	expiresAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	queryer := &credentialQueryerStub{row: credentialRow{
		values: []any{
			"tenant_01",
			bytesOf(0x7a, sha256.Size),
			[]byte(`["capabilities:read","job:submit"]`),
			sql.NullTime{Time: expiresAt, Valid: true},
			sql.NullTime{},
		},
	}}
	store := &SQLCredentialStore{queryer: queryer}
	credential, err := store.FindCredentialByPrefix(context.Background(), "public12")
	if err != nil {
		t.Fatal(err)
	}
	if credential.TenantID != "tenant_01" || len(credential.Digest) != sha256.Size || credential.ExpiresAt == nil || !credential.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("credential = %+v", credential)
	}
	if len(credential.Scopes) != 2 || credential.Scopes[0] != ScopeCapabilitiesRead || credential.Scopes[1] != ScopeJobSubmit {
		t.Fatalf("scopes = %#v", credential.Scopes)
	}
	query := strings.ToLower(queryer.query)
	if !strings.Contains(query, "t_external_api_key") || !strings.Contains(query, "t_external_tenant") || !strings.Contains(query, "tenant.status = 'active'") {
		t.Fatalf("credential query does not enforce active tenant: %s", queryer.query)
	}
}

func TestSQLCredentialStoreTreatsUnknownPrefixAsAbsent(t *testing.T) {
	store := &SQLCredentialStore{queryer: &credentialQueryerStub{row: credentialRow{err: sql.ErrNoRows}}}
	credential, err := store.FindCredentialByPrefix(context.Background(), "unknown12")
	if err != nil || credential != nil {
		t.Fatalf("credential=%+v err=%v", credential, err)
	}
}

func TestSQLCredentialStoreFailsClosedForInvalidStoredCredentials(t *testing.T) {
	tests := map[string][]any{
		"empty tenant":    {"", bytesOf(1, sha256.Size), []byte(`["job:read"]`), sql.NullTime{}, sql.NullTime{}},
		"bad digest":      {"tenant_01", []byte("short"), []byte(`["job:read"]`), sql.NullTime{}, sql.NullTime{}},
		"bad json":        {"tenant_01", bytesOf(1, sha256.Size), []byte(`{"job:read":true}`), sql.NullTime{}, sql.NullTime{}},
		"unknown scope":   {"tenant_01", bytesOf(1, sha256.Size), []byte(`["root:all"]`), sql.NullTime{}, sql.NullTime{}},
		"duplicate scope": {"tenant_01", bytesOf(1, sha256.Size), []byte(`["job:read","job:read"]`), sql.NullTime{}, sql.NullTime{}},
	}
	for name, values := range tests {
		t.Run(name, func(t *testing.T) {
			store := &SQLCredentialStore{queryer: &credentialQueryerStub{row: credentialRow{values: values}}}
			if _, err := store.FindCredentialByPrefix(context.Background(), "public12"); err == nil {
				t.Fatal("expected invalid stored credential rejection")
			}
		})
	}
}

type credentialQueryerStub struct {
	query string
	row   credentialRow
}

func (queryer *credentialQueryerStub) QueryRowContext(_ context.Context, query string, _ ...any) rowScanner {
	queryer.query = query
	return queryer.row
}

type credentialRow struct {
	values []any
	err    error
}

func (row credentialRow) Scan(destinations ...any) error {
	if row.err != nil {
		return row.err
	}
	if len(destinations) != len(row.values) {
		return fmt.Errorf("destination count = %d", len(destinations))
	}
	for index, value := range row.values {
		switch destination := destinations[index].(type) {
		case *string:
			*destination = value.(string)
		case *[]byte:
			*destination = append([]byte(nil), value.([]byte)...)
		case *sql.NullTime:
			*destination = value.(sql.NullTime)
		default:
			return errors.New("unexpected destination type")
		}
	}
	return nil
}

func bytesOf(value byte, count int) []byte {
	return []byte(strings.Repeat(string([]byte{value}), count))
}
