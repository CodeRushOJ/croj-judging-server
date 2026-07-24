package admincli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/external"
)

type provisionerStub struct {
	tenantName    string
	tenantPolicy  external.TenantPolicy
	tenantID      string
	scopes        []external.Scope
	expiresAt     *time.Time
	pepper        []byte
	material      external.APIKeyMaterial
	keyCalls      int
	callbackURL   string
	callback      external.CallbackMaterial
	callbackCalls int
}

func (stub *provisionerStub) CreateCallback(_ context.Context, tenantID, destinationURL string) (external.CallbackMaterial, error) {
	stub.tenantID, stub.callbackURL = tenantID, destinationURL
	stub.callbackCalls++
	return stub.callback, nil
}

func (stub *provisionerStub) CreateTenant(_ context.Context, name string, policy external.TenantPolicy) (string, error) {
	stub.tenantName, stub.tenantPolicy = name, policy
	return "ceirceirceirceirceirceirce", nil
}

func (stub *provisionerStub) CreateAPIKey(_ context.Context, tenantID string, scopes []external.Scope, expiresAt *time.Time, pepper []byte) (external.APIKeyMaterial, error) {
	stub.tenantID, stub.scopes, stub.expiresAt, stub.pepper = tenantID, scopes, expiresAt, pepper
	stub.keyCalls++
	return stub.material, nil
}

func TestRunCreatesATenantWithExplicitPolicy(t *testing.T) {
	stub := &provisionerStub{}
	var output bytes.Buffer
	err := Run(context.Background(), []string{
		"tenant", "create", "--name", "Acme OJ",
		"--max-queued", "80", "--max-running", "4", "--max-source-bytes", "1048576",
		"--max-bundles", "120", "--daily-execution-ms", "3600000", "--max-infra-tries", "3",
	}, stub, nil, &output)
	if err != nil {
		t.Fatal(err)
	}
	if stub.tenantName != "Acme OJ" || stub.tenantPolicy.MaxQueuedJobs != 80 || stub.tenantPolicy.MaxRunningJobs != 4 || stub.tenantPolicy.MaxInfrastructureTries != 3 {
		t.Fatalf("tenant request = %q %+v", stub.tenantName, stub.tenantPolicy)
	}
	if output.String() != "Tenant created: ceirceirceirceirceirceirce\n" {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunCreatesAKeyAndPrintsTheSecretExactlyOnce(t *testing.T) {
	plaintext := "croj_public12_0123456789012345678901234567890123456789012"
	stub := &provisionerStub{material: external.APIKeyMaterial{Plaintext: plaintext, LookupPrefix: "public12"}}
	pepper := bytes.Repeat([]byte{0x55}, 32)
	var output bytes.Buffer
	err := Run(context.Background(), []string{
		"api-key", "create",
		"--tenant", "ceirceirceirceirceirceirce",
		"--scopes", "capabilities:read,job:submit,job:read",
		"--expires-at", "2027-01-01T00:00:00Z",
	}, stub, pepper, &output)
	if err != nil {
		t.Fatal(err)
	}
	if stub.keyCalls != 1 || stub.tenantID != "ceirceirceirceirceirceirce" || len(stub.scopes) != 3 || stub.expiresAt == nil || !bytes.Equal(stub.pepper, pepper) {
		t.Fatalf("key request tenant=%q scopes=%#v expiry=%v", stub.tenantID, stub.scopes, stub.expiresAt)
	}
	if strings.Count(output.String(), plaintext) != 1 || !strings.Contains(output.String(), "shown once") {
		t.Fatalf("secret output = %q", output.String())
	}
}

func TestRunCreatesACallbackAndPrintsTheSecretExactlyOnce(t *testing.T) {
	secret := "croj_whsec_0123456789012345678901234567890123456789012"
	stub := &provisionerStub{callback: external.CallbackMaterial{CallbackID: "deirceirceirceirceirceirce", Secret: secret}}
	var output bytes.Buffer
	err := Run(context.Background(), []string{
		"callback", "create", "--tenant", "ceirceirceirceirceirceirce", "--url", "https://oj.example.com/hook",
	}, stub, nil, &output)
	if err != nil {
		t.Fatal(err)
	}
	if stub.callbackCalls != 1 || stub.tenantID != "ceirceirceirceirceirceirce" || stub.callbackURL != "https://oj.example.com/hook" {
		t.Fatalf("callback request tenant=%q url=%q calls=%d", stub.tenantID, stub.callbackURL, stub.callbackCalls)
	}
	if strings.Count(output.String(), secret) != 1 || strings.Count(output.String(), "deirceirceirceirceirceirce") != 1 || !strings.Contains(output.String(), "shown once") {
		t.Fatalf("callback output = %q", output.String())
	}
}

func TestRunRejectsInvalidCallbackFlagsBeforeProvisioning(t *testing.T) {
	for name, arguments := range map[string][]string{
		"missing tenant": {"callback", "create", "--url", "https://oj.example.com/hook"},
		"missing URL":    {"callback", "create", "--tenant", "ceirceirceirceirceirceirce"},
		"positional":     {"callback", "create", "extra", "--tenant", "ceirceirceirceirceirceirce", "--url", "https://oj.example.com/hook"},
	} {
		t.Run(name, func(t *testing.T) {
			stub := &provisionerStub{}
			if err := Run(context.Background(), arguments, stub, nil, &bytes.Buffer{}); err == nil || stub.callbackCalls != 0 {
				t.Fatalf("error=%v calls=%d", err, stub.callbackCalls)
			}
		})
	}
}

func TestRunRejectsUnknownOrDuplicateScopesBeforeProvisioning(t *testing.T) {
	for name, scopes := range map[string]string{
		"unknown":   "root:all",
		"duplicate": "job:read,job:read",
	} {
		t.Run(name, func(t *testing.T) {
			stub := &provisionerStub{}
			err := Run(context.Background(), []string{"api-key", "create", "--tenant", "ceirceirceirceirceirceirce", "--scopes", scopes}, stub, make([]byte, 32), &bytes.Buffer{})
			if err == nil || stub.keyCalls != 0 {
				t.Fatalf("error=%v calls=%d", err, stub.keyCalls)
			}
		})
	}
}
