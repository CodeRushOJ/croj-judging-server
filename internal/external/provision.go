package external

import (
	"context"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"time"
)

const externalIDRandomBytes = 16

var externalIDPattern = regexp.MustCompile(`^[a-z2-7]{26}$`)

type TenantPolicy struct {
	MaxQueuedJobs          int   `json:"maxQueuedJobs"`
	MaxRunningJobs         int   `json:"maxRunningJobs"`
	MaxSourceBytes         int64 `json:"maxSourceBytes"`
	MaxRetainedBundles     int   `json:"maxRetainedBundles"`
	DailyExecutionMillis   int64 `json:"dailyExecutionMillis"`
	MaxInfrastructureTries int   `json:"maxInfrastructureTries"`
	MaxTimeLimitMillis     int   `json:"maxTimeLimitMillis"`
	MaxMemoryLimitMiB      int   `json:"maxMemoryLimitMiB"`
}

func (policy TenantPolicy) validate() error {
	if policy.MaxQueuedJobs <= 0 || policy.MaxRunningJobs <= 0 || policy.MaxRunningJobs > policy.MaxQueuedJobs {
		return fmt.Errorf("queued/running job limits are invalid")
	}
	if policy.MaxSourceBytes <= 0 || policy.MaxSourceBytes > MaximumSourceBytes || policy.MaxRetainedBundles <= 0 || policy.DailyExecutionMillis <= 0 {
		return fmt.Errorf("source limit must be between 1 and %d bytes; bundle and daily execution limits must be positive", MaximumSourceBytes)
	}
	if policy.MaxInfrastructureTries <= 0 || policy.MaxInfrastructureTries > 10 {
		return fmt.Errorf("infrastructure attempt limit must be between 1 and 10")
	}
	if policy.MaxTimeLimitMillis <= 0 || policy.MaxMemoryLimitMiB <= 0 {
		return fmt.Errorf("maximum execution time and memory limits must be positive")
	}
	return nil
}

type provisionExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type Provisioner struct {
	executor         provisionExecutor
	random           io.Reader
	now              func() time.Time
	callbackCipher   *CallbackCipher
	callbackResolver callbackResolver
}

type ProvisionerOption func(*Provisioner) error

func WithCallbackCipher(callbackCipher *CallbackCipher) ProvisionerOption {
	return func(provisioner *Provisioner) error {
		if callbackCipher == nil {
			return fmt.Errorf("callback cipher is required")
		}
		provisioner.callbackCipher = callbackCipher
		return nil
	}
}

func WithCallbackResolver(resolver callbackResolver) ProvisionerOption {
	return func(provisioner *Provisioner) error {
		if resolver == nil {
			return fmt.Errorf("callback resolver is required")
		}
		provisioner.callbackResolver = resolver
		return nil
	}
}

func NewProvisioner(database *sql.DB, random io.Reader, options ...ProvisionerOption) (*Provisioner, error) {
	if database == nil || random == nil {
		return nil, fmt.Errorf("provisioning database and cryptographic random source are required")
	}
	provisioner := &Provisioner{executor: database, random: random, now: time.Now, callbackResolver: net.DefaultResolver}
	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("provisioner option is required")
		}
		if err := option(provisioner); err != nil {
			return nil, err
		}
	}
	return provisioner, nil
}

func (provisioner *Provisioner) CreateTenant(ctx context.Context, name string, policy TenantPolicy) (string, error) {
	if provisioner == nil || provisioner.executor == nil || provisioner.random == nil {
		return "", fmt.Errorf("provisioner is not configured")
	}
	name = strings.TrimSpace(name)
	if len(name) < 2 || len(name) > 128 {
		return "", fmt.Errorf("tenant name must contain 2 to 128 bytes")
	}
	if err := policy.validate(); err != nil {
		return "", err
	}
	tenantID, err := generateExternalID(provisioner.random)
	if err != nil {
		return "", err
	}
	encodedPolicy, err := json.Marshal(policy)
	if err != nil {
		return "", fmt.Errorf("encode tenant policy: %w", err)
	}
	if _, err := provisioner.executor.ExecContext(ctx,
		"INSERT INTO t_external_tenant(external_id, name, status, policy_json) VALUES (?, ?, 'ACTIVE', ?)",
		tenantID, name, encodedPolicy); err != nil {
		return "", fmt.Errorf("create tenant: %w", err)
	}
	return tenantID, nil
}

func (provisioner *Provisioner) CreateAPIKey(
	ctx context.Context,
	tenantID string,
	scopes []Scope,
	expiresAt *time.Time,
	pepper []byte,
) (APIKeyMaterial, error) {
	if provisioner == nil || provisioner.executor == nil || provisioner.random == nil {
		return APIKeyMaterial{}, fmt.Errorf("provisioner is not configured")
	}
	if !externalIDPattern.MatchString(tenantID) {
		return APIKeyMaterial{}, fmt.Errorf("tenant ID is invalid")
	}
	encodedScopes, err := json.Marshal(scopes)
	if err != nil {
		return APIKeyMaterial{}, fmt.Errorf("encode API key scopes: %w", err)
	}
	if _, err := decodeScopes(encodedScopes); err != nil {
		return APIKeyMaterial{}, err
	}
	now := time.Now
	if provisioner.now != nil {
		now = provisioner.now
	}
	if expiresAt != nil && !expiresAt.After(now()) {
		return APIKeyMaterial{}, fmt.Errorf("API key expiry must be in the future")
	}
	material, err := GenerateAPIKey(provisioner.random, pepper)
	if err != nil {
		return APIKeyMaterial{}, err
	}
	result, err := provisioner.executor.ExecContext(ctx, `
INSERT INTO t_external_api_key(tenant_id, lookup_prefix, key_digest, scopes_json, expires_at)
SELECT tenant.id, ?, ?, ?, ?
FROM t_external_tenant AS tenant
WHERE tenant.external_id = ? AND tenant.status = 'ACTIVE'`,
		material.LookupPrefix, material.Digest, encodedScopes, expiresAt, tenantID)
	if err != nil {
		return APIKeyMaterial{}, fmt.Errorf("create API key: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return APIKeyMaterial{}, fmt.Errorf("confirm API key creation: %w", err)
	}
	if affected != 1 {
		return APIKeyMaterial{}, fmt.Errorf("tenant does not exist or is disabled")
	}
	return material, nil
}

func (provisioner *Provisioner) CreateCallback(ctx context.Context, tenantID, rawDestination string) (CallbackMaterial, error) {
	if provisioner == nil || provisioner.executor == nil || provisioner.random == nil || provisioner.callbackCipher == nil || provisioner.callbackResolver == nil {
		return CallbackMaterial{}, fmt.Errorf("callback provisioner is not configured")
	}
	if !externalIDPattern.MatchString(tenantID) {
		return CallbackMaterial{}, fmt.Errorf("tenant ID is invalid")
	}
	destination, err := CanonicalCallbackDestination(rawDestination)
	if err != nil {
		return CallbackMaterial{}, err
	}
	if _, err := resolvePublicCallback(ctx, provisioner.callbackResolver, destination.Host); err != nil {
		return CallbackMaterial{}, fmt.Errorf("validate callback public destination: %w", err)
	}
	callbackID, err := generateExternalID(provisioner.random)
	if err != nil {
		return CallbackMaterial{}, err
	}
	secretEntropy := make([]byte, 32)
	if _, err := io.ReadFull(provisioner.random, secretEntropy); err != nil {
		return CallbackMaterial{}, fmt.Errorf("generate callback secret: %w", err)
	}
	secret := "croj_whsec_" + base64.RawURLEncoding.EncodeToString(secretEntropy)
	clear(secretEntropy)
	secretBytes := []byte(secret)
	encrypted, err := provisioner.callbackCipher.Encrypt(tenantID, callbackID, destination.URL, secretBytes)
	clear(secretBytes)
	if err != nil {
		return CallbackMaterial{}, err
	}
	defer clear(encrypted.Ciphertext)
	defer clear(encrypted.Nonce)
	result, err := provisioner.executor.ExecContext(ctx, `
INSERT INTO t_external_callback(
    external_id, tenant_id, destination_url, allowed_host, allowed_port,
    secret_ciphertext, secret_nonce, secret_key_version
)
SELECT ?, tenant.id, ?, ?, ?, ?, ?, ?
FROM t_external_tenant AS tenant
WHERE tenant.external_id = ? AND tenant.status = 'ACTIVE'`,
		callbackID, destination.URL, destination.Host, destination.Port,
		encrypted.Ciphertext, encrypted.Nonce, encrypted.KeyVersion, tenantID)
	if err != nil {
		return CallbackMaterial{}, fmt.Errorf("create callback: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return CallbackMaterial{}, fmt.Errorf("confirm callback creation: %w", err)
	}
	if affected != 1 {
		return CallbackMaterial{}, fmt.Errorf("tenant does not exist or is disabled")
	}
	return CallbackMaterial{CallbackID: callbackID, Secret: secret}, nil
}

func generateExternalID(random io.Reader) (string, error) {
	buffer := make([]byte, externalIDRandomBytes)
	if _, err := io.ReadFull(random, buffer); err != nil {
		return "", fmt.Errorf("generate external ID: %w", err)
	}
	value := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buffer))
	if !externalIDPattern.MatchString(value) {
		return "", fmt.Errorf("generated external ID is invalid")
	}
	return value, nil
}
