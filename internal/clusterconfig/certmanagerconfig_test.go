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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/stretchr/testify/assert"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	certManagerCRD = &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "certificates.cert-manager.io",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:     "Certificate",
				ListKind: "CertificateList",
			},
		},
	}

	testCertificate1 = &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cert-manager.io/v1",
			"kind":       "Certificate",
			"metadata": map[string]any{
				"name":      "my-cert",
				"namespace": "default",
			},
			"spec": map[string]any{
				"secretName": "my-cert-tls",
				"issuerRef": map[string]any{
					"name": "letsencrypt-prod",
					"kind": "ClusterIssuer",
				},
			},
		},
	}

	testCertificate2 = &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cert-manager.io/v1",
			"kind":       "Certificate",
			"metadata": map[string]any{
				"name":      "other-cert",
				"namespace": "default",
			},
			"spec": map[string]any{
				"secretName": "my-cert-tls", // same secret as testCertificate1
				"issuerRef": map[string]any{
					"name": "ca-issuer",
					"kind": "Issuer",
				},
			},
		},
	}

	testTLSSecret = &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]any{
				"name":      "my-cert-tls",
				"namespace": "default",
			},
			"type": "kubernetes.io/tls",
			"data": map[string]any{
				"tls.crt": "dGVzdC1jZXJ0",
				"tls.key": "dGVzdC1rZXk=",
			},
		},
	}

	testCertificateCustomNS = &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cert-manager.io/v1",
			"kind":       "Certificate",
			"metadata": map[string]any{
				"name":      "custom-cert",
				"namespace": "ibu-test-ns",
			},
			"spec": map[string]any{
				"secretName": "custom-cert-tls",
				"issuerRef": map[string]any{
					"name": "selfsigned-issuer",
					"kind": "ClusterIssuer",
				},
			},
		},
	}

	testTLSSecretCustomNS = &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]any{
				"name":      "custom-cert-tls",
				"namespace": "ibu-test-ns",
			},
			"type": "kubernetes.io/tls",
			"data": map[string]any{
				"tls.crt": "dGVzdC1jZXJ0",
				"tls.key": "dGVzdC1rZXk=",
			},
		},
	}

	testClusterIssuerLetsencrypt = &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cert-manager.io/v1",
			"kind":       "ClusterIssuer",
			"metadata": map[string]any{
				"name": "letsencrypt-prod",
			},
			"spec": map[string]any{
				"acme": map[string]any{
					"server": "https://acme-v02.api.letsencrypt.org/directory",
				},
			},
		},
	}

	testClusterIssuerSelfsigned = &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cert-manager.io/v1",
			"kind":       "ClusterIssuer",
			"metadata": map[string]any{
				"name": "selfsigned-issuer",
			},
			"spec": map[string]any{
				"selfSigned": map[string]any{},
			},
		},
	}

	testIssuerCA = &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cert-manager.io/v1",
			"kind":       "Issuer",
			"metadata": map[string]any{
				"name":      "ca-issuer",
				"namespace": "default",
			},
			"spec": map[string]any{
				"ca": map[string]any{
					"secretName": "ca-key-pair",
				},
			},
		},
	}
)

func init() {
	// Generate a self-signed certificate with CN so that writeCertManagerCrypto
	// recognises it as valid for recert use_cert rules.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test.example.com"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{"test.example.com"},
	}
	certDER, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	certB64 := base64.StdEncoding.EncodeToString(certPEM)
	keyB64 := base64.StdEncoding.EncodeToString(keyPEM)

	testTLSSecret.Object["data"].(map[string]any)["tls.crt"] = certB64
	testTLSSecret.Object["data"].(map[string]any)["tls.key"] = keyB64
	testTLSSecretCustomNS.Object["data"].(map[string]any)["tls.crt"] = certB64
	testTLSSecretCustomNS.Object["data"].(map[string]any)["tls.key"] = keyB64
}

func TestFetchCertManagerConfig(t *testing.T) {
	testcases := []struct {
		name         string
		objs         []client.Object
		validateFunc func(t *testing.T, manifestsDir string)
	}{
		{
			name: "cert-manager CRD not found",
			objs: []client.Object{},
			validateFunc: func(t *testing.T, manifestsDir string) {
				manifests, err := os.ReadDir(manifestsDir)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				assert.Equal(t, 0, len(manifests))
			},
		},
		{
			name: "all resources present",
			objs: []client.Object{certManagerCRD, testCertificate1, testTLSSecret, testClusterIssuerLetsencrypt},
			validateFunc: func(t *testing.T, manifestsDir string) {
				manifests, err := os.ReadDir(manifestsDir)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				// Secret (03_) only — Certificate CRs not exported (webhook not ready post-pivot)
				assert.Equal(t, 1, len(manifests))
				assert.Contains(t, manifests[0].Name(), "03_certmanager_Secret")

				// Verify cert-manager crypto files for recert use_cert rules
				cryptoDir := filepath.Join(filepath.Dir(manifestsDir), common.CertManagerCryptoDir)
				cryptoFiles, err := os.ReadDir(cryptoDir)
				assert.NoError(t, err)
				assert.Equal(t, 1, len(cryptoFiles), "expected 1 .crt file for the TLS Secret")
				assert.Contains(t, cryptoFiles[0].Name(), "default_my-cert-tls.crt")

				// Verify the cert file contains valid PEM data
				certData, err := os.ReadFile(filepath.Join(cryptoDir, cryptoFiles[0].Name()))
				assert.NoError(t, err)
				assert.True(t, strings.HasPrefix(string(certData), "-----BEGIN CERTIFICATE-----"),
					"cert file should contain PEM-encoded certificate")
			},
		},
		{
			name: "certificate but secret missing",
			objs: []client.Object{certManagerCRD, testCertificate1, testClusterIssuerLetsencrypt},
			validateFunc: func(t *testing.T, manifestsDir string) {
				manifests, err := os.ReadDir(manifestsDir)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				// No manifests — Secret not found and Certificate CRs not exported
				assert.Equal(t, 0, len(manifests))
			},
		},
		{
			name: "CRD exists but no resources",
			objs: []client.Object{certManagerCRD},
			validateFunc: func(t *testing.T, manifestsDir string) {
				manifests, err := os.ReadDir(manifestsDir)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				assert.Equal(t, 0, len(manifests))
			},
		},
		{
			name: "multiple certificates referencing same secret",
			objs: []client.Object{certManagerCRD, testCertificate1, testCertificate2, testTLSSecret,
				testClusterIssuerLetsencrypt, testIssuerCA},
			validateFunc: func(t *testing.T, manifestsDir string) {
				manifests, err := os.ReadDir(manifestsDir)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				// Secret (03_, deduplicated) only — Certificate CRs not exported
				assert.Equal(t, 1, len(manifests))
				assert.Contains(t, manifests[0].Name(), "03_certmanager_Secret")
			},
		},
		{
			name: "custom namespace gets exported",
			objs: []client.Object{certManagerCRD, testCertificateCustomNS, testTLSSecretCustomNS,
				testClusterIssuerSelfsigned},
			validateFunc: func(t *testing.T, manifestsDir string) {
				manifests, err := os.ReadDir(manifestsDir)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				// Namespace (01_) + Secret (03_) — Certificate CRs not exported
				assert.Equal(t, 2, len(manifests))
				assert.Contains(t, manifests[0].Name(), "01_certmanager_Namespace_ibu-test-ns")
				assert.Contains(t, manifests[1].Name(), "03_certmanager_Secret")
			},
		},
		{
			name: "mixed namespaces: default skipped, custom exported",
			objs: []client.Object{certManagerCRD, testCertificate1, testTLSSecret,
				testCertificateCustomNS, testTLSSecretCustomNS,
				testClusterIssuerLetsencrypt, testClusterIssuerSelfsigned},
			validateFunc: func(t *testing.T, manifestsDir string) {
				manifests, err := os.ReadDir(manifestsDir)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				// 1 Namespace (ibu-test-ns) + 2 Secrets (default + ibu-test-ns)
				// Certificate CRs not exported
				assert.Equal(t, 3, len(manifests))
				assert.Contains(t, manifests[0].Name(), "01_certmanager_Namespace_ibu-test-ns")
				assert.Contains(t, manifests[1].Name(), "03_certmanager_Secret")
				assert.Contains(t, manifests[2].Name(), "03_certmanager_Secret")
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			manifestsDir := filepath.Join(tmpDir, common.OptOpenshift, common.ClusterConfigDir, ManifestDir)

			if err := os.MkdirAll(manifestsDir, 0o700); err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			c := fake.NewClientBuilder().WithScheme(testscheme).WithObjects(tc.objs...).Build()
			ucc := &UpgradeClusterConfigGather{
				Client: c,
				Log:    logr.Discard(),
				Scheme: c.Scheme(),
			}

			err := ucc.FetchCertManagerConfig(context.Background(), tmpDir)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			tc.validateFunc(t, manifestsDir)
		})
	}
}

func TestCleanResource(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cert-manager.io/v1",
			"kind":       "Certificate",
			"metadata": map[string]any{
				"name":              "my-cert",
				"namespace":         "default",
				"uid":               "12345-abcde",
				"resourceVersion":   "999",
				"generation":        int64(3),
				"creationTimestamp":  "2026-01-01T00:00:00Z",
				"ownerReferences":   []any{map[string]any{"name": "owner"}},
				"managedFields":     []any{map[string]any{"manager": "test"}},
			},
			"spec": map[string]any{
				"secretName": "my-cert-tls",
			},
			"status": map[string]any{
				"conditions": []any{map[string]any{"type": "Ready"}},
			},
		},
	}

	CleanResource(obj)

	// Verify transient fields are removed
	assert.Empty(t, obj.GetUID())
	assert.Empty(t, obj.GetResourceVersion())
	assert.Equal(t, int64(0), obj.GetGeneration())
	assert.Nil(t, obj.GetOwnerReferences())
	assert.Nil(t, obj.GetManagedFields())

	// Verify status is removed
	_, hasStatus := obj.Object["status"]
	assert.False(t, hasStatus, "status should be removed")

	// Verify creationTimestamp is removed from metadata
	metadata := obj.Object["metadata"].(map[string]any)
	_, hasTimestamp := metadata["creationTimestamp"]
	assert.False(t, hasTimestamp, "creationTimestamp should be removed")

	// Verify spec content is preserved
	spec := obj.Object["spec"].(map[string]any)
	assert.Equal(t, "my-cert-tls", spec["secretName"])

	// Verify the cleaned resource can be marshaled to valid JSON
	data, err := json.Marshal(obj.Object)
	assert.NoError(t, err)
	assert.NotEmpty(t, data)
}
