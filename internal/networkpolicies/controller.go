package networkpolicies

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/openshift-kni/lifecycle-agent/internal/precache"
	"github.com/openshift-kni/lifecycle-agent/internal/prep"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const defaultDenyTmpl = `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny
  namespace: %s
spec:
  podSelector: {}
  policyTypes:
    - Ingress
    - Egress
`

const controllerTmpl = `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: controller-manager
  namespace: %s
spec:
  podSelector:
    matchLabels:
      control-plane: controller-manager
  egress:
    - ports:
      - protocol: TCP
        port: %s
      - protocol: TCP
        port: 53
      - protocol: UDP
        port: 53
      - protocol: TCP
        port: 443
  ingress:
    - ports:
      - protocol: TCP
        port: %s
  policyTypes:
    - Egress
    - Ingress
`

const jobTmpl = `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  podSelector:
    matchLabels:
      job-name: %[1]s
  egress:
    - ports:
      - protocol: TCP
        port: %[3]s
      - protocol: TCP
        port: 53
      - protocol: UDP
        port: 53
      - protocol: TCP
        port: 443
  policyTypes:
    - Egress
`

type Policy struct {
	Namespace  string
	MetricAddr string
}

func parsePolicy(template string, args ...any) (*networkingv1.NetworkPolicy, error) {
	yamlStr := fmt.Sprintf(template, args...)
	networkPolicy := &networkingv1.NetworkPolicy{}
	if err := yaml.Unmarshal([]byte(yamlStr), networkPolicy); err != nil {
		return nil, fmt.Errorf("failed to unmarshal NetworkPolicy YAML: %w", err)
	}

	return networkPolicy, nil
}

func (p *Policy) InstallPolicies(cfg *rest.Config) (string, error) {
	p0, err0 := parsePolicy(controllerTmpl, p.Namespace, parsePort(cfg.Host), parsePort(p.MetricAddr))
	p1, err2 := parsePolicy(jobTmpl, prep.StaterootSetupJobName, p.Namespace, parsePort(cfg.Host))
	p2, err3 := parsePolicy(jobTmpl, precache.LcaPrecacheResourceName, p.Namespace, parsePort(cfg.Host))
	p3, err1 := parsePolicy(defaultDenyTmpl, p.Namespace)
	if err := cmp.Or(err0, err1, err2, err3); err != nil {
		return "", fmt.Errorf("failed to create NetworkPolicy from template: %w", err)
	}
	policies := []*networkingv1.NetworkPolicy{p0, p1, p2, p3}

	c, err := client.New(cfg, client.Options{})
	if err != nil {
		return "", fmt.Errorf("failed to create direct client: %w", err)
	}

	ctx := context.Background()
	result := []string{}
	for _, p := range policies {
		if err := c.Create(ctx, p); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("failed to create NetworkPolicy: %w", err)
		}
		result = append(result, p.GetName())
	}

	return "NetworkPolicies Installed: " + strings.Join(result, ", "), nil
}

func Check(cfg *rest.Config, ns string) string {
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		return fmt.Sprintf("failed to create k8s client: %v", err)
	}

	policies := &networkingv1.NetworkPolicyList{}
	if err := c.List(context.Background(), policies, client.InNamespace(ns)); err != nil {
		return fmt.Sprintf("failed to list network policies in namespace %s: %v", ns, err)
	}

	if len(policies.Items) > 0 {
		var names []string
		for _, p := range policies.Items {
			names = append(names, p.GetName())
		}
		return fmt.Sprintf("NetworkPolicies %v detected in namespace [%s] - these may interfere with traffic flow", names, ns)
	}

	return ""
}

func parsePort(address string) string {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return "6443"
	}

	return port
}
