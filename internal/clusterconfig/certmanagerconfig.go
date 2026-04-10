/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package clusterconfig

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/utils"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch

const certManagerCRDName = "certificates.cert-manager.io"

// PreserveCertManagerConfig exports cert-manager TLS Secrets to the new stateroot so they
// are restored post-pivot via applyManifests. Exported resources:
//
//   - Namespaces (01_ prefix) — for non-standard namespaces containing cert-manager resources
//   - TLS Secrets (03_ prefix) — paired with Certificate CRs
//
// Certificate CRs, ClusterIssuers, and Issuers are NOT exported because the
// cert-manager validating webhook is not yet running when applyManifests executes
// post-pivot, and any attempt to apply cert-manager custom resources would fail.
// Recert handles Certificate CR hostname/IP updates via cn_san_replace_rules.
//
// The private keys from TLS Secrets are also written to the certmanager-crypto
// directory and used as recert use_key rules to preserve cert-manager key material
// through recert's re-keying process (only for certificates with a CommonName).
func (r *UpgradeClusterConfigGather) PreserveCertManagerConfig(ctx context.Context, ostreeDir string) error {
	r.Log.Info("Preserving cert-manager configuration", "ostreeDir", ostreeDir)

	// Check if cert-manager is installed by looking for the Certificate CRD
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := r.Get(ctx, types.NamespacedName{Name: certManagerCRDName}, crd); err != nil {
		if k8serrors.IsNotFound(err) {
			r.Log.Info("cert-manager CRD not found, skipping cert-manager configuration export",
				"crdName", certManagerCRDName)
			return nil
		}
		return fmt.Errorf("failed to get cert-manager CRD: %w", err)
	}
	r.Log.Info("cert-manager CRD found", "crdName", certManagerCRDName,
		"crdUID", crd.GetUID(), "versions", len(crd.Spec.Versions))

	manifestsDir := filepath.Join(ostreeDir, common.OptOpenshift, common.ClusterConfigDir, ManifestDir)

	// Ensure the manifests directory exists (defensive — FetchClusterConfig should have created it)
	if err := os.MkdirAll(manifestsDir, 0o700); err != nil {
		return fmt.Errorf("failed to create manifests dir %s: %w", manifestsDir, err)
	}

	secretRefs, err := collectCertManagerSecretRefs(ctx, r.Log, r.Client)
	if err != nil {
		return fmt.Errorf("failed to collect cert-manager secret refs: %w", err)
	}

	secrets, err := fetchCertManagerSecrets(ctx, r.Log, r.Client, secretRefs)
	if err != nil {
		return fmt.Errorf("failed to fetch cert-manager secrets: %w", err)
	}

	if err := writeCertManagerCrypto(r.Log, ostreeDir, secrets); err != nil {
		return fmt.Errorf("failed to write cert-manager crypto for recert: %w", err)
	}

	namespacesWritten, err := exportCertManagerNamespaces(r.Log, manifestsDir, secrets)
	if err != nil {
		return fmt.Errorf("failed to export cert-manager namespaces: %w", err)
	}

	secretsWritten, err := exportCertManagerSecrets(r.Log, manifestsDir, secrets)
	if err != nil {
		return fmt.Errorf("failed to export cert-manager secrets: %w", err)
	}

	r.Log.Info("Successfully preserved cert-manager configuration",
		"namespaces", namespacesWritten,
		"secrets", secretsWritten,
		"totalFilesWritten", namespacesWritten+secretsWritten)
	return nil
}

// collectCertManagerSecretRefs lists cert-manager Certificate CRs and returns the
// set of Secret references (namespace/name) they point to, deduplicated.
func collectCertManagerSecretRefs(
	ctx context.Context, log logr.Logger, reader client.Reader,
) (map[types.NamespacedName]bool, error) {
	log.Info("Listing cert-manager Certificates")
	certList := &unstructured.UnstructuredList{}
	certList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "cert-manager.io",
		Kind:    "CertificateList",
		Version: "v1",
	})
	if err := reader.List(ctx, certList); err != nil {
		return nil, fmt.Errorf("failed to list Certificates: %w", err)
	}
	log.Info("Listed cert-manager Certificates", "count", len(certList.Items))

	secretRefs := make(map[types.NamespacedName]bool)
	for i := range certList.Items {
		cert := &certList.Items[i]
		log.Info("Processing Certificate", "name", cert.GetName(), "namespace", cert.GetNamespace())
		spec, ok := cert.Object["spec"].(map[string]any)
		if ok {
			if secretName, exists := spec["secretName"].(string); exists && secretName != "" {
				log.Info("Certificate references Secret",
					"certificate", cert.GetName(), "secretName", secretName, "namespace", cert.GetNamespace())
				secretRefs[types.NamespacedName{
					Name:      secretName,
					Namespace: cert.GetNamespace(),
				}] = true
			}
		}
	}
	log.Info("Collected secret references from Certificates", "secretRefCount", len(secretRefs))
	return secretRefs, nil
}

// fetchCertManagerSecrets fetches each referenced Secret from the API server,
// skipping any that are not found.
func fetchCertManagerSecrets(
	ctx context.Context, log logr.Logger, reader client.Reader,
	secretRefs map[types.NamespacedName]bool,
) (map[types.NamespacedName]*unstructured.Unstructured, error) {
	secrets := make(map[types.NamespacedName]*unstructured.Unstructured, len(secretRefs))
	for ref := range secretRefs {
		secret := &unstructured.Unstructured{}
		secret.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "",
			Kind:    "Secret",
			Version: "v1",
		})
		if err := reader.Get(ctx, ref, secret); err != nil {
			if k8serrors.IsNotFound(err) {
				log.Info("Secret referenced by Certificate not found, skipping",
					"secret", ref.Name, "namespace", ref.Namespace)
				continue
			}
			return nil, fmt.Errorf("failed to get Secret %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		secrets[ref] = secret
	}
	return secrets, nil
}

// writeCertManagerCrypto extracts private keys from cert-manager TLS Secrets and writes
// them to the certmanager-crypto directory. These files are used as recert use_key rules
// to preserve cert-manager key material through recert's re-keying. The CN is extracted
// from the paired certificate and encoded in the filename for use_key rule construction.
func writeCertManagerCrypto(
	log logr.Logger, ostreeDir string, secrets map[types.NamespacedName]*unstructured.Unstructured,
) error {
	if len(secrets) == 0 {
		return nil
	}

	cryptoDir := filepath.Join(ostreeDir, common.OptOpenshift, common.ClusterConfigDir, common.CertManagerCryptoDir)
	if err := os.MkdirAll(cryptoDir, 0o700); err != nil {
		return fmt.Errorf("failed to create certmanager crypto dir %s: %w", cryptoDir, err)
	}

	for ref, secret := range secrets {
		data, ok := secret.Object["data"].(map[string]any)
		if !ok {
			log.Info("Secret has no data field, skipping crypto extraction",
				"secret", ref.Name, "namespace", ref.Namespace)
			continue
		}

		// Extract CN from the certificate — recert use_key rules require a CN for matching.
		tlsCrt, crtOk := data["tls.crt"].(string)
		if !crtOk {
			log.Info("Secret has no tls.crt field, skipping crypto extraction",
				"secret", ref.Name, "namespace", ref.Namespace)
			continue
		}
		decodedCrt, err := base64.StdEncoding.DecodeString(tlsCrt)
		if err != nil {
			return fmt.Errorf("failed to decode tls.crt for %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		block, _ := pem.Decode(decodedCrt)
		if block == nil {
			log.Info("Could not decode PEM block from tls.crt, skipping crypto extraction",
				"secret", ref.Name, "namespace", ref.Namespace)
			continue
		}
		parsedCert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			log.Info("Could not parse X.509 certificate, skipping crypto extraction",
				"secret", ref.Name, "namespace", ref.Namespace, "error", err)
			continue
		}
		if parsedCert.Subject.CommonName == "" {
			log.Info("Certificate has no CN, skipping recert use_key rule (will be restored via manifest)",
				"secret", ref.Name, "namespace", ref.Namespace)
			continue
		}

		// Write the private key as a PEM file for recert use_key rules.
		// The CN is encoded in the filename so appendCertManagerCryptoRules can
		// reconstruct the "CN path/to/key" format that recert expects.
		tlsKey, keyOk := data["tls.key"].(string)
		if !keyOk {
			log.Info("Secret has no tls.key field, skipping crypto extraction",
				"secret", ref.Name, "namespace", ref.Namespace)
			continue
		}
		decodedKey, err := base64.StdEncoding.DecodeString(tlsKey)
		if err != nil {
			return fmt.Errorf("failed to decode tls.key for %s/%s: %w", ref.Namespace, ref.Name, err)
		}

		// Filename format: "CN=<cn>__<namespace>_<name>.key"
		// The CN= prefix and __ separator allow appendCertManagerCryptoRules to parse the CN.
		keyFile := filepath.Join(cryptoDir, fmt.Sprintf("CN=%s__%s_%s.key",
			parsedCert.Subject.CommonName, ref.Namespace, ref.Name))
		if err := os.WriteFile(keyFile, decodedKey, 0o600); err != nil {
			return fmt.Errorf("failed to write key file %s: %w", keyFile, err)
		}
		log.Info("Wrote cert-manager TLS key for recert preservation",
			"secret", ref.Name, "namespace", ref.Namespace, "path", keyFile,
			"cn", parsedCert.Subject.CommonName)
	}

	log.Info("Wrote cert-manager crypto for recert preservation", "secretCount", len(secrets))
	return nil
}

// exportCertManagerNamespaces writes Namespace manifests (01_ prefix) for non-standard
// namespaces containing cert-manager resources. Well-known namespaces (openshift-*, kube-*,
// default, openshift) are skipped as they already exist in the seed stateroot.
func exportCertManagerNamespaces(
	log logr.Logger, manifestsDir string, secrets map[types.NamespacedName]*unstructured.Unstructured,
) (int, error) {
	namespacesWritten := 0
	exportedNamespaces := make(map[string]bool)
	for ref := range secrets {
		ns := ref.Namespace
		if exportedNamespaces[ns] {
			continue
		}
		exportedNamespaces[ns] = true

		// Skip well-known namespaces that will already exist in the seed stateroot
		if strings.HasPrefix(ns, "openshift-") || strings.HasPrefix(ns, "kube-") || ns == "default" || ns == "openshift" {
			log.Info("Skipping well-known namespace for cert-manager resource", "namespace", ns)
			continue
		}

		log.Info("Exporting Namespace for cert-manager resource", "namespace", ns)
		nsObj := &unstructured.Unstructured{}
		nsObj.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Kind: "Namespace", Version: "v1"})
		nsObj.SetName(ns)
		fileName := fmt.Sprintf("01_certmanager_Namespace_%s.json", ns)
		filePath := filepath.Join(manifestsDir, fileName)
		if err := utils.MarshalToFile(nsObj.Object, filePath); err != nil {
			return 0, fmt.Errorf("failed to write Namespace manifest to %s: %w", filePath, err)
		}
		namespacesWritten++
	}
	log.Info("Exported cert-manager namespaces", "count", namespacesWritten)
	return namespacesWritten, nil
}

// exportCertManagerSecrets cleans and writes TLS Secret manifests (03_ prefix)
// so they are restored via applyManifests before cert-manager starts.
func exportCertManagerSecrets(
	log logr.Logger, manifestsDir string, secrets map[types.NamespacedName]*unstructured.Unstructured,
) (int, error) {
	secretsWritten := 0
	for ref, secret := range secrets {
		CleanResource(secret)
		fileName := fmt.Sprintf("03_certmanager_Secret_%s_%s.json", ref.Name, ref.Namespace)
		filePath := filepath.Join(manifestsDir, fileName)
		log.Info("Writing cert-manager Secret to file", "path", filePath)
		if err := utils.MarshalToFile(secret.Object, filePath); err != nil {
			return 0, fmt.Errorf("failed to write Secret to %s: %w", filePath, err)
		}
		secretsWritten++
	}
	return secretsWritten, nil
}

// CleanResource removes transient metadata fields from an unstructured resource
// so it can be cleanly re-applied post-pivot.
func CleanResource(obj *unstructured.Unstructured) {
	obj.SetUID("")
	obj.SetResourceVersion("")
	obj.SetManagedFields(nil)
	obj.SetGeneration(0)
	obj.SetOwnerReferences(nil)
	delete(obj.Object, "status")

	if metadata, ok := obj.Object["metadata"].(map[string]any); ok {
		delete(metadata, "creationTimestamp")
	}
}
