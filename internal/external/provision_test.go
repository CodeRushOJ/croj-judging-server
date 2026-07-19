package external

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestProvisionerCreatesTenantWithValidatedPolicy(t *testing.T) {
	executor := &provisionExecutorStub{affected: 1}
	provisioner := &Provisioner{executor: executor, random: bytes.NewReader(bytes.Repeat([]byte{0x11}, externalIDRandomBytes))}
	policy := TenantPolicy{
		MaxQueuedJobs:          100,
		MaxRunningJobs:         4,
		MaxSourceBytes:         1 << 20,
		MaxRetainedBundles:     200,
		DailyExecutionMillis:   3_600_000,
		MaxInfrastructureTries: 3,
	}
	tenantID, err := provisioner.CreateTenant(context.Background(), "Acme OJ", policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(tenantID) != 26 || strings.ToLower(tenantID) != tenantID {
		t.Fatalf("tenant id = %q", tenantID)
	}
	if !strings.Contains(strings.ToLower(executor.query), "insert into t_external_tenant") || len(executor.arguments) != 3 {
		t.Fatalf("execution: %s %#v", executor.query, executor.arguments)
	}
	if executor.arguments[0] != tenantID || executor.arguments[1] != "Acme OJ" || !json.Valid(executor.arguments[2].([]byte)) {
		t.Fatalf("arguments = %#v", executor.arguments)
	}
}

func TestProvisionerCreatesAPIKeyOnceForAnActiveTenant(t *testing.T) {
	executor := &provisionExecutorStub{affected: 1}
	random := bytes.NewReader(bytes.Repeat([]byte{0x22}, apiKeyRandomBytes))
	provisioner := &Provisioner{
		executor: executor,
		random:   random,
		now:      func() time.Time { return time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC) },
	}
	pepper := bytes.Repeat([]byte{0x33}, sha256.Size)
	expires := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	material, err := provisioner.CreateAPIKey(
		context.Background(),
		"ceirceirceirceirceirceirce",
		[]Scope{ScopeCapabilitiesRead, ScopeJobSubmit},
		&expires,
		pepper,
	)
	if err != nil {
		t.Fatal(err)
	}
	if material.Plaintext == "" || material.LookupPrefix == "" || len(material.Digest) != sha256.Size {
		t.Fatalf("material = %s", material)
	}
	if !strings.Contains(strings.ToLower(executor.query), "insert into t_external_api_key") || !strings.Contains(strings.ToLower(executor.query), "tenant.status = 'active'") {
		t.Fatalf("query = %s", executor.query)
	}
	if len(executor.arguments) != 5 || executor.arguments[0] != material.LookupPrefix || executor.arguments[4] != "ceirceirceirceirceirceirce" {
		t.Fatalf("arguments = %#v", executor.arguments)
	}
	if string(executor.arguments[2].([]byte)) != `["capabilities:read","job:submit"]` {
		t.Fatalf("scopes JSON = %s", executor.arguments[2])
	}
}

func TestProvisionerRejectsUnsafePolicyScopesAndUnknownTenant(t *testing.T) {
	validPolicy := TenantPolicy{MaxQueuedJobs: 1, MaxRunningJobs: 1, MaxSourceBytes: 1024, MaxRetainedBundles: 1, DailyExecutionMillis: 1000, MaxInfrastructureTries: 1}
	for name, policy := range map[string]TenantPolicy{
		"zero queue":         {MaxRunningJobs: 1, MaxSourceBytes: 1, MaxRetainedBundles: 1, DailyExecutionMillis: 1, MaxInfrastructureTries: 1},
		"running over queue": {MaxQueuedJobs: 1, MaxRunningJobs: 2, MaxSourceBytes: 1, MaxRetainedBundles: 1, DailyExecutionMillis: 1, MaxInfrastructureTries: 1},
	} {
		t.Run(name, func(t *testing.T) {
			provisioner := &Provisioner{executor: &provisionExecutorStub{affected: 1}, random: bytes.NewReader(make([]byte, externalIDRandomBytes))}
			if _, err := provisioner.CreateTenant(context.Background(), "Acme OJ", policy); err == nil {
				t.Fatal("expected policy rejection")
			}
		})
	}
	unknownTenant := &Provisioner{executor: &provisionExecutorStub{affected: 0}, random: bytes.NewReader(make([]byte, apiKeyRandomBytes))}
	if _, err := unknownTenant.CreateAPIKey(context.Background(), "ceirceirceirceirceirceirce", []Scope{ScopeJobRead}, nil, make([]byte, sha256.Size)); err == nil {
		t.Fatal("expected unknown/disabled tenant rejection")
	}
	duplicateScope := &Provisioner{executor: &provisionExecutorStub{affected: 1}, random: bytes.NewReader(make([]byte, apiKeyRandomBytes))}
	if _, err := duplicateScope.CreateAPIKey(context.Background(), "ceirceirceirceirceirceirce", []Scope{ScopeJobRead, ScopeJobRead}, nil, make([]byte, sha256.Size)); err == nil {
		t.Fatal("expected duplicate scope rejection")
	}
	if _, err := (&Provisioner{}).CreateTenant(context.Background(), "Acme OJ", validPolicy); err == nil {
		t.Fatal("expected unconfigured provisioner rejection")
	}
}

type provisionExecutorStub struct {
	query     string
	arguments []any
	affected  int64
	err       error
}

func (executor *provisionExecutorStub) ExecContext(_ context.Context, query string, arguments ...any) (sql.Result, error) {
	executor.query = query
	executor.arguments = arguments
	return stubResult(executor.affected), executor.err
}
