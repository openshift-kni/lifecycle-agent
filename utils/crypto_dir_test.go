package utils

import (
	"context"
	"os"
	"path"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
)

func TestBackupKubeadminPasswordHash(t *testing.T) {
	tests := []struct {
		name           string
		secret         *corev1.Secret
		expectedFound  bool
		expectedErr    bool
		expectedErrMsg string
	}{
		{
			name: "secret exists",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "kubeadmin",
					Namespace: "kube-system",
				},
				Data: map[string][]byte{
					"kubeadmin": []byte("$2a$10$hashed-password"),
				},
			},
			expectedFound: true,
		},
		{
			name:          "secret not found returns false with no error",
			secret:        nil,
			expectedFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			builder := fake.NewClientBuilder().WithScheme(scheme.Scheme)
			if tt.secret != nil {
				builder = builder.WithObjects(tt.secret)
			}
			cl := builder.Build()

			found, err := BackupKubeadminPasswordHash(context.Background(), cl, dir)
			if tt.expectedErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.expectedFound, found)

			hashFile := path.Join(dir, pwHashFile)
			if tt.expectedFound {
				content, readErr := os.ReadFile(hashFile)
				assert.NoError(t, readErr)
				assert.Equal(t, string(tt.secret.Data["kubeadmin"]), string(content))
			} else {
				_, statErr := os.Stat(hashFile)
				assert.True(t, os.IsNotExist(statErr))
			}
		})
	}
}

func TestLoadKubeadminPasswordHash(t *testing.T) {
	tests := []struct {
		name         string
		writeContent string
		writeFile    bool
		expectedHash string
		expectedErr  bool
	}{
		{
			name:         "file exists with content",
			writeFile:    true,
			writeContent: "$2a$10$hashed-password",
			expectedHash: "$2a$10$hashed-password",
		},
		{
			name:         "file does not exist returns empty string",
			writeFile:    false,
			expectedHash: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.writeFile {
				assert.NoError(t, os.WriteFile(path.Join(dir, pwHashFile), []byte(tt.writeContent), 0o600))
			}

			hash, err := LoadKubeadminPasswordHash(dir)
			if tt.expectedErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.expectedHash, hash)
		})
	}
}

func TestKubeadminPasswordHash_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	expectedHash := "$2a$10$round-trip-test-hash"

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kubeadmin",
			Namespace: "kube-system",
		},
		Data: map[string][]byte{
			"kubeadmin": []byte(expectedHash),
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(secret).Build()

	found, err := BackupKubeadminPasswordHash(context.Background(), cl, dir)
	assert.NoError(t, err)
	assert.True(t, found)

	loaded, err := LoadKubeadminPasswordHash(dir)
	assert.NoError(t, err)
	assert.Equal(t, expectedHash, loaded)
}

func TestSeedReconfigurationKubeconfigRetentionToCryptoDir(t *testing.T) {
	dir := t.TempDir()
	cryptoDir := path.Join(dir, "kubeconfig-crypto")

	retention := &seedreconfig.KubeConfigCryptoRetention{
		KubeAPICrypto: seedreconfig.KubeAPICrypto{
			ServingCrypto: seedreconfig.ServingCrypto{
				LoadbalancerSignerPrivateKey:   "lb-key-pem",
				LocalhostSignerPrivateKey:      "localhost-key-pem",
				ServiceNetworkSignerPrivateKey: "svc-net-key-pem",
			},
			ClientAuthCrypto: seedreconfig.ClientAuthCrypto{
				AdminCACertificate: "admin-ca-cert-pem",
			},
		},
		IngresssCrypto: seedreconfig.IngresssCrypto{
			IngressCAPrivateKey: "ingress-key-pem",
		},
	}

	err := SeedReconfigurationKubeconfigRetentionToCryptoDir(cryptoDir, retention)
	assert.NoError(t, err)

	expectedFiles := map[string]string{
		"admin-kubeconfig-client-ca.crt":     "admin-ca-cert-pem",
		"loadbalancer-serving-signer.key":    "lb-key-pem",
		"localhost-serving-signer.key":       "localhost-key-pem",
		"service-network-serving-signer.key": "svc-net-key-pem",
		"ingresskey-ingress-operator.key":    "ingress-key-pem",
	}

	for filename, expectedContent := range expectedFiles {
		filepath := path.Join(cryptoDir, filename)
		content, readErr := os.ReadFile(filepath)
		assert.NoError(t, readErr, "failed to read %s", filename)
		assert.Equal(t, expectedContent, string(content), "content mismatch for %s", filename)

		info, statErr := os.Stat(filepath)
		assert.NoError(t, statErr)
		assert.Equal(t, os.FileMode(cryptoDirMode), info.Mode().Perm(), "wrong permissions for %s", filename)
	}
}

func TestSeedReconfigurationKubeconfigRetentionToCryptoDir_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	cryptoDir := path.Join(dir, "nested", "crypto")

	retention := &seedreconfig.KubeConfigCryptoRetention{}
	err := SeedReconfigurationKubeconfigRetentionToCryptoDir(cryptoDir, retention)
	assert.NoError(t, err)

	info, err := os.Stat(cryptoDir)
	assert.NoError(t, err)
	assert.True(t, info.IsDir())
}
