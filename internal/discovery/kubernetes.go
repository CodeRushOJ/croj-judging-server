package discovery

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	typeddiscoveryv1 "k8s.io/client-go/kubernetes/typed/discovery/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// KubernetesDiscovery resolves ready sandbox Pods from EndpointSlices owned by
// a Kubernetes Service. Service traffic can still use native kube-proxy load
// balancing; direct endpoints are exposed for judge-side scheduling decisions.
type KubernetesDiscovery struct {
	endpointSlices typeddiscoveryv1.EndpointSliceInterface
	serviceName    string
	portName       string
}

func NewKubernetesDiscovery(namespace, serviceName, portName, kubeconfig string) (*KubernetesDiscovery, error) {
	if namespace == "" || serviceName == "" || portName == "" {
		return nil, fmt.Errorf("sandbox namespace, service and port name are required")
	}
	config, err := kubernetesConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	client, err := typeddiscoveryv1.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	return &KubernetesDiscovery{
		endpointSlices: client.EndpointSlices(namespace),
		serviceName:    serviceName,
		portName:       portName,
	}, nil
}

func kubernetesConfig(kubeconfig string) (*rest.Config, error) {
	config, err := rest.InClusterConfig()
	if err == nil {
		return config, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules,
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load in-cluster credentials or kubeconfig: %w", err)
	}
	return config, nil
}

func (d *KubernetesDiscovery) Endpoints(ctx context.Context) ([]string, error) {
	selector := labels.Set{discoveryv1.LabelServiceName: d.serviceName}.AsSelector().String()
	list, err := d.endpointSlices.List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list EndpointSlices for Service %s: %w", d.serviceName, err)
	}
	return EndpointAddresses(list.Items, d.portName)
}

// EndpointAddresses is kept pure so readiness and termination behavior can be
// verified without a live Kubernetes API server.
func EndpointAddresses(slices []discoveryv1.EndpointSlice, portName string) ([]string, error) {
	addresses := make(map[string]struct{})
	for _, slice := range slices {
		port, ok := endpointPort(slice.Ports, portName)
		if !ok {
			continue
		}
		for _, endpoint := range slice.Endpoints {
			if endpoint.Conditions.Ready == nil || !*endpoint.Conditions.Ready {
				continue
			}
			if endpoint.Conditions.Terminating != nil && *endpoint.Conditions.Terminating {
				continue
			}
			for _, address := range endpoint.Addresses {
				addresses[net.JoinHostPort(address, strconv.Itoa(int(port)))] = struct{}{}
			}
		}
	}

	result := make([]string, 0, len(addresses))
	for address := range addresses {
		result = append(result, address)
	}
	sort.Strings(result)
	return result, nil
}

func endpointPort(ports []discoveryv1.EndpointPort, name string) (int32, bool) {
	for _, port := range ports {
		if port.Name == nil || *port.Name != name || port.Port == nil {
			continue
		}
		if port.Protocol != nil && *port.Protocol != corev1.ProtocolTCP {
			continue
		}
		return *port.Port, true
	}
	return 0, false
}
