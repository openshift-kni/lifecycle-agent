package extramanifest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	policyv1 "open-cluster-management.io/config-policy-controller/api/v1"
	policiesv1 "open-cluster-management.io/governance-policy-propagator/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups=policy.open-cluster-management.io,resources=policies,verbs=get;list;watch

// This annotation helps with the ordering as well as whether the policy should be applied. This is the same expectation as
// the normal ZTP process post non-image-based cluster installation.
const ztpDeployWaveAnnotation = "ran.openshift.io/ztp-deploy-wave"
const TargetOcpVersionManifestCountAnnotation = "lca.openshift.io/target-ocp-version-manifest-count"
const TargetOcpVersionLabel = "lca.openshift.io/target-ocp-version"

// GetPolicies gets the policies matching the labels from the namespace and sort them by the ztp wave annotation value
func (h *EMHandler) GetPolicies(ctx context.Context, labels map[string]string) ([]*policiesv1.Policy, error) {
	listOpts := []client.ListOption{
		client.MatchingLabels(labels),
	}

	policies := &policiesv1.PolicyList{}
	if err := h.Client.List(ctx, policies, listOpts...); err != nil {
		return nil, fmt.Errorf("failed to list policies: %w", err)
	}

	var policyWaveMap = make(map[*policiesv1.Policy]int)

	for i := range policies.Items {
		policy := &policies.Items[i]
		// CRs from enforce mode policy gets applied by ACM policy engine
		// We only care about the inform ones that typically get enforced by TALM through the ZTP CGU
		if strings.EqualFold(string(policy.Spec.RemediationAction), "enforce") {
			h.Log.Info(fmt.Sprintf("Ignoring policy %s with remediationAction enforce", policy.Name))
			continue
		}

		deployWave, found := policy.GetAnnotations()[ztpDeployWaveAnnotation]
		if found {
			deployWaveInt, err := strconv.Atoi(deployWave)
			if err != nil {
				// err convert from string to int
				h.Log.Error(err, "Annotation "+ztpDeployWaveAnnotation+" is not an interger", "policy name", policy.GetName())
			}
			_, err = getParentPolicyNameAndNamespace(policy.GetName())
			if err != nil {
				h.Log.Info(fmt.Sprintf("Ignoring policy %s with invalid name", policy.Name))
				continue
			}
			policyWaveMap[policy] = deployWaveInt
		} else {
			h.Log.Info(fmt.Sprintf("Ignoring policy %s without the %s annotation", policy.Name, ztpDeployWaveAnnotation))
		}
	}

	return sortPolicyMap(policyWaveMap), nil
}

// Gets encapsulated objects from policy
func getConfigurationObjects(log *logr.Logger, policy *policiesv1.Policy, objectLabels map[string]string) ([]*unstructured.Unstructured, error) {
	var uobjects []*unstructured.Unstructured

	var objects []runtime.RawExtension

	if !policy.Spec.Disabled {
		for _, template := range policy.Spec.PolicyTemplates {
			o := *template.ObjectDefinition.DeepCopy()
			objects = append(objects, o)
		}
	}

	for _, ob := range objects {
		var pol policyv1.ConfigurationPolicy
		err := json.Unmarshal(ob.DeepCopy().Raw, &pol)
		if err != nil {
			return uobjects, fmt.Errorf("failed to unmarshal ConfigurationPolicy: %w", err)
		}
		for _, ot := range pol.Spec.ObjectTemplates {
			if strings.EqualFold(string(ot.ComplianceType), string(policyv1.MustNotHave)) {
				continue
			}

			var object unstructured.Unstructured
			err = object.UnmarshalJSON(ot.ObjectDefinition.DeepCopy().Raw)
			if err != nil {
				return uobjects, fmt.Errorf("failed to unmarshal ObjectTemplate: %w", err)
			}

			if len(objectLabels) > 0 {
				if metadata, exists := object.Object["metadata"].(map[string]interface{}); exists {
					if labels, exists := metadata["labels"].(map[string]interface{}); exists {
						labelsFound := true
						for oLabel, oValue := range objectLabels {
							value, exists := labels[oLabel]
							if !exists {
								labelsFound = false
								break
							}

							if oLabel == TargetOcpVersionLabel {
								expectedValues := strings.Split(oValue, ",")
								if !lo.Contains(expectedValues, value.(string)) {
									log.Info(fmt.Sprintf("Found %s label in manifest %s %s, but its value %s is not in %v, skipping", oLabel, object.GetKind(), object.GetName(), value, expectedValues))
									labelsFound = false
									break
								}
							} else if oValue != value.(string) {
								labelsFound = false
								break
							}
						}
						if !labelsFound {
							continue
						}
						annotations, exists := metadata["annotations"].(map[string]interface{})
						if !exists {
							annotations = make(map[string]interface{})
						}
						applyType := getApplyType(string(ot.ComplianceType))
						if applyType == "" {
							return uobjects, fmt.Errorf("unsupported compliance type: %s", ot.ComplianceType)
						}
						annotations[common.ApplyTypeAnnotation] = applyType
					} else {
						continue
					}
				} else {
					continue
				}
			}

			object.Object["status"] = map[string]interface{}{} // remove status, we can't apply it
			uobjects = append(uobjects, &object)
		}
	}
	return uobjects, nil
}

func getApplyType(complianceType string) string {
	var applyType string
	if strings.EqualFold(complianceType, string(policyv1.MustHave)) {
		applyType = common.ApplyTypeMerge
	} else if strings.EqualFold(complianceType, string(policyv1.MustOnlyHave)) {
		applyType = common.ApplyTypeReplace
	}
	return applyType
}

// getParentPolicyNameAndNamespace gets the parent policy name and namespace from a given child policy
// returns: []string       a two-element slice which the first element is policy namespace and the second one is policy name
func getParentPolicyNameAndNamespace(childPolicyName string) ([]string, error) {
	// The format of a child policy name is parent_policy_namespace.parent_policy_name.
	// Extract the parent policy name and namespace by splitting the child policy name into two substrings separated by "."
	// and we are safe to split with the separator "." as the namespace is disallowed to contain "."
	res := strings.SplitN(childPolicyName, ".", 2)
	if len(res) != 2 {
		return nil, errors.New("child policy name " + childPolicyName + " is not valid.")
	}
	return res, nil
}

func sortPolicyMap(sortMap map[*policiesv1.Policy]int) []*policiesv1.Policy {
	var keys []*policiesv1.Policy
	for key := range sortMap {
		keys = append(keys, key)
	}

	sort.Slice(keys, func(i, j int) bool {
		// for equal elements, sort string alphabetically
		if sortMap[keys[i]] == sortMap[keys[j]] {
			return keys[i].Name < keys[j].Name
		}
		return sortMap[keys[i]] < sortMap[keys[j]]
	})
	return keys
}
