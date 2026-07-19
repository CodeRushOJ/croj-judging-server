package external

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"net/netip"
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

func TestProvisionerCreatesEncryptedCallbackForPublicDestination(t *testing.T) {
	executor := &provisionExecutorStub{affected: 1}
	callbackCipher, err := NewCallbackCipher(7, map[uint16][]byte{7: bytes.Repeat([]byte{0x71}, 32)}, bytes.NewReader(bytes.Repeat([]byte{0x72}, 12)))
	if err != nil {
		t.Fatal(err)
	}
	provisioner := &Provisioner{
		executor:         executor,
		random:           bytes.NewReader(bytes.Repeat([]byte{0x22}, externalIDRandomBytes+32)),
		callbackCipher:   callbackCipher,
		callbackResolver: callbackResolverStub{addresses: []netip.Addr{netip.MustParseAddr("8.8.8.8")}},
	}
	material, err := provisioner.CreateCallback(context.Background(), "ceirceirceirceirceirceirce", "HTTPS://OJ.Example.com/hooks?b=2&a=1")
	if err != nil {
		t.Fatal(err)
	}
	if !externalIDPattern.MatchString(material.CallbackID) || !strings.HasPrefix(material.Secret, "croj_whsec_") {
		t.Fatalf("material = %s", material)
	}
	if !strings.Contains(strings.ToLower(executor.query), "insert into t_external_callback") || !strings.Contains(strings.ToLower(executor.query), "tenant.status = 'active'") {
		t.Fatalf("query = %s", executor.query)
	}
	if len(executor.arguments) != 8 || executor.arguments[0] != material.CallbackID || executor.arguments[1] != "https://oj.example.com:443/hooks?a=1&b=2" || executor.arguments[2] != "oj.example.com" || executor.arguments[3] != uint16(443) || executor.arguments[6] != uint16(7) || executor.arguments[7] != "ceirceirceirceirceirceirce" {
		t.Fatalf("arguments = %#v", executor.arguments)
	}
	ciphertext, ok := executor.arguments[4].([]byte)
	if !ok || bytes.Contains(ciphertext, []byte(material.Secret)) {
		t.Fatal("callback secret was not encrypted")
	}
	nonce, ok := executor.arguments[5].([]byte)
	if !ok || len(nonce) != 12 {
		t.Fatalf("nonce = %#v", executor.arguments[5])
	}
}

func TestProvisionerRejectsUnsafeCallbackBeforePersistence(t *testing.T) {
	callbackCipher, err := NewCallbackCipher(1, map[uint16][]byte{1: bytes.Repeat([]byte{1}, 32)}, bytes.NewReader(bytes.Repeat([]byte{2}, 12)))
	if err != nil {
		t.Fatal(err)
	}
	for name, resolver := range map[string]callbackResolverStub{
		"private": {addresses: []netip.Addr{netip.MustParseAddr("10.0.0.1")}},
		"mixed":   {addresses: []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}},
		"empty":   {},
	} {
		t.Run(name, func(t *testing.T) {
			executor := &provisionExecutorStub{affected: 1}
			provisioner := &Provisioner{
				executor: executor, random: bytes.NewReader(bytes.Repeat([]byte{3}, externalIDRandomBytes+32)),
				callbackCipher: callbackCipher, callbackResolver: resolver,
			}
			if _, err := provisioner.CreateCallback(context.Background(), "ceirceirceirceirceirceirce", "https://oj.example.com/hook"); err == nil || executor.query != "" {
				t.Fatalf("error=%v query=%q", err, executor.query)
			}
		})
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
	validPolicy.MaxSourceBytes = MaximumSourceBytes + 1
	if err := validPolicy.validate(); err == nil {
		t.Fatal("policy accepted a source larger than the encrypted object contract")
	}
	validPolicy.MaxSourceBytes = MaximumSourceBytes
	if err := validPolicy.validate(); err != nil {
		t.Fatalf("maximum supported source size was rejected: %v", err)
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

type callbackResolverStub struct {
	addresses []netip.Addr
	err       error
}

func (resolver callbackResolverStub) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return append([]netip.Addr(nil), resolver.addresses...), resolver.err
}

func (executor *provisionExecutorStub) ExecContext(_ context.Context, query string, arguments ...any) (sql.Result, error) {
	executor.query = query
	executor.arguments = arguments
	return stubResult(executor.affected), executor.err
}
