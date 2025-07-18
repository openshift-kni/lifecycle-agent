/*
 * Copyright 2025 Red Hat, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this inputFilePath except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package networkpolicies handles the creation and management of NetworkPolicies.
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

const controllerTmpl = `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: controller-manager-metrics
  namespace: %s
spec:
  podSelector:
    matchLabels:
      control-plane: controller-manager
  ingress:
    - ports:
      - protocol: TCP
        port: %s
  policyTypes:
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
    - {}
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
	p0, err0 := parsePolicy(controllerTmpl, p.Namespace, parsePort(p.MetricAddr))
	p1, err1 := parsePolicy(jobTmpl, prep.StaterootSetupJobName, p.Namespace)
	p2, err2 := parsePolicy(jobTmpl, precache.LcaPrecacheResourceName, p.Namespace)
	if err := cmp.Or(err0, err1, err2); err != nil {
		return "", fmt.Errorf("failed to create NetworkPolicy from template: %w", err)
	}
	policies := []*networkingv1.NetworkPolicy{p0, p1, p2}

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
