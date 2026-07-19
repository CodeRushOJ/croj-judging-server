package admincli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/external"
)

type Provisioner interface {
	CreateTenant(context.Context, string, external.TenantPolicy) (string, error)
	CreateAPIKey(context.Context, string, []external.Scope, *time.Time, []byte) (external.APIKeyMaterial, error)
}

func Run(ctx context.Context, arguments []string, provisioner Provisioner, pepper []byte, output io.Writer) error {
	if provisioner == nil || output == nil {
		return fmt.Errorf("provisioner and output are required")
	}
	if len(arguments) < 2 {
		return fmt.Errorf("usage: judge-admin <tenant|api-key> create [flags]")
	}
	switch arguments[0] + " " + arguments[1] {
	case "tenant create":
		return createTenant(ctx, arguments[2:], provisioner, output)
	case "api-key create":
		return createAPIKey(ctx, arguments[2:], provisioner, pepper, output)
	default:
		return fmt.Errorf("unsupported command %q", strings.Join(arguments[:2], " "))
	}
}

func createTenant(ctx context.Context, arguments []string, provisioner Provisioner, output io.Writer) error {
	flags := flag.NewFlagSet("tenant create", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("name", "", "tenant display name")
	policy := external.TenantPolicy{}
	flags.IntVar(&policy.MaxQueuedJobs, "max-queued", 100, "maximum queued jobs")
	flags.IntVar(&policy.MaxRunningJobs, "max-running", 4, "maximum running jobs")
	flags.Int64Var(&policy.MaxSourceBytes, "max-source-bytes", 1<<20, "maximum source bytes")
	flags.IntVar(&policy.MaxRetainedBundles, "max-bundles", 200, "maximum retained bundles")
	flags.Int64Var(&policy.DailyExecutionMillis, "daily-execution-ms", 3_600_000, "daily execution budget")
	flags.IntVar(&policy.MaxInfrastructureTries, "max-infra-tries", 3, "maximum infrastructure attempts")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("parse tenant flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("tenant create does not accept positional arguments")
	}
	tenantID, err := provisioner.CreateTenant(ctx, *name, policy)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Tenant created: %s\n", tenantID)
	return err
}

func createAPIKey(ctx context.Context, arguments []string, provisioner Provisioner, pepper []byte, output io.Writer) error {
	flags := flag.NewFlagSet("api-key create", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	tenantID := flags.String("tenant", "", "tenant external ID")
	encodedScopes := flags.String("scopes", "", "comma-separated scopes")
	encodedExpiry := flags.String("expires-at", "", "optional RFC3339 expiry")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("parse API key flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("api-key create does not accept positional arguments")
	}
	scopes, err := parseScopes(*encodedScopes)
	if err != nil {
		return err
	}
	var expiresAt *time.Time
	if *encodedExpiry != "" {
		parsed, err := time.Parse(time.RFC3339, *encodedExpiry)
		if err != nil {
			return fmt.Errorf("parse API key expiry: %w", err)
		}
		expiresAt = &parsed
	}
	material, err := provisioner.CreateAPIKey(ctx, *tenantID, scopes, expiresAt, pepper)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "API key (shown once): %s\n", material.Plaintext)
	return err
}

func parseScopes(encoded string) ([]external.Scope, error) {
	if strings.TrimSpace(encoded) == "" {
		return nil, fmt.Errorf("at least one scope is required")
	}
	allowed := map[external.Scope]struct{}{
		external.ScopeCapabilitiesRead: {},
		external.ScopeBundleWrite:      {},
		external.ScopeBundleRead:       {},
		external.ScopeJobSubmit:        {},
		external.ScopeJobRead:          {},
		external.ScopeJobCancel:        {},
	}
	values := strings.Split(encoded, ",")
	scopes := make([]external.Scope, 0, len(values))
	seen := make(map[external.Scope]struct{}, len(values))
	for _, value := range values {
		scope := external.Scope(strings.TrimSpace(value))
		if _, valid := allowed[scope]; !valid {
			return nil, fmt.Errorf("unknown scope %q", scope)
		}
		if _, duplicate := seen[scope]; duplicate {
			return nil, fmt.Errorf("duplicate scope %q", scope)
		}
		seen[scope] = struct{}{}
		scopes = append(scopes, scope)
	}
	return scopes, nil
}
