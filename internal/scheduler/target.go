package scheduler

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Target delegates endpoint discovery and per-RPC balancing to gRPC. The DNS
// name must belong to a headless Service; a normal ClusterIP is only one A
// record and is not client-side endpoint balancing.
type Target struct{ target string }

func NewTarget(target string) (*Target, error) {
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme != "dns" || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("sandbox gRPC target must use dns:///service.namespace.svc.cluster.local:port")
	}
	host, port, err := net.SplitHostPort(strings.TrimPrefix(parsed.Path, "/"))
	if err != nil || port == "" || !strings.HasSuffix(host, ".svc.cluster.local") {
		return nil, fmt.Errorf("sandbox gRPC target must name a Kubernetes headless Service and port")
	}
	return &Target{target: target}, nil
}

func (selector *Target) SelectSandbox() (string, error) {
	if selector == nil || selector.target == "" {
		return "", fmt.Errorf("sandbox gRPC target is unavailable")
	}
	return selector.target, nil
}

func (selector *Target) SelectSandboxExcluding(map[string]struct{}) (string, error) {
	// Reusing the logical target is intentional: gRPC round_robin chooses among
	// its resolved Pod endpoints for each retry.
	return selector.SelectSandbox()
}
