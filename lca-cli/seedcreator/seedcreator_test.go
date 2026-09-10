package seedcreator

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	mcv1 "github.com/openshift/api/machineconfiguration/v1"
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
)

var (
	testScheme         = runtime.NewScheme()
	testTLSKeyPEM      = mustKeyPEM()
	testIngressCertPEM = mustCertPEM("ingress-operator@123456")
	testMCOConfigData  []byte
)

func init() {
	_ = clientgoscheme.AddToScheme(testScheme)
	_ = configv1.AddToScheme(testScheme)
	_ = mcv1.AddToScheme(testScheme)
	_ = operatorv1alpha1.AddToScheme(testScheme)
	_ = operatorsv1alpha1.AddToScheme(testScheme)

	data, err := os.ReadFile(filepath.Join("testdata", "mco-currentconfig.json"))
	if err != nil {
		panic(err)
	}
	testMCOConfigData = data
}

func testLogger() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(os.Stderr)
	return log
}

func mustCertPEM(cn string) []byte {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
}

func mustKeyPEM() []byte {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func clusterFixtureObjects() []client.Object {
	const mcName = "rendered-master-test"
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "master-1",
			Labels: map[string]string{
				"node-role.kubernetes.io/master": "",
			},
			Annotations: map[string]string{
				"machineconfiguration.openshift.io/currentConfig": mcName,
			},
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
				{Type: corev1.NodeHostName, Address: "seed-node"},
			},
		},
	}

	installConfig := `baseDomain: example.com
metadata:
  name: test-cluster
networking:
  machineNetwork:
  - cidr: 192.168.1.0/24
`

	objs := []client.Object{
		&configv1.ClusterVersion{
			ObjectMeta: metav1.ObjectMeta{Name: "version"},
			Spec:       configv1.ClusterVersionSpec{ClusterID: "test-cluster-id"},
			Status: configv1.ClusterVersionStatus{
				Desired: configv1.Release{Version: "4.18.0"},
			},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: common.InstallConfigCM, Namespace: common.InstallConfigCMNamespace},
			Data:       map[string]string{"install-config": installConfig},
		},
		node,
		&configv1.Proxy{ObjectMeta: metav1.ObjectMeta{Name: common.OpenshiftProxyCRName}},
		&configv1.Network{
			ObjectMeta: metav1.ObjectMeta{Name: common.OpenshiftInfraCRName},
			Status: configv1.NetworkStatus{
				ClusterNetwork: []configv1.ClusterNetworkEntry{{CIDR: "10.128.0.0/14"}},
				ServiceNetwork: []string{"172.30.0.0/16"},
			},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: common.CsvDeploymentName, Namespace: common.CsvDeploymentNamespace},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Image: "quay.io/openshift-release-dev/ocp-v4.0-art-dev:latest"}},
					},
				},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "router-ca", Namespace: "openshift-ingress-operator"},
			Data: map[string][]byte{
				"tls.crt": testIngressCertPEM,
				"tls.key": testTLSKeyPEM,
			},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "admin-kubeconfig-client-ca", Namespace: common.OpenshiftConfigNamespace},
			Data:       map[string]string{"ca-bundle.crt": "ca-data"},
		},
		&mcv1.MachineConfig{
			ObjectMeta: metav1.ObjectMeta{Name: mcName},
			Spec:       mcv1.MachineConfigSpec{FIPS: false},
		},
	}
	for _, prefix := range common.CertPrefixes {
		objs = append(objs, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: prefix, Namespace: "openshift-kube-apiserver-operator"},
			Data:       map[string][]byte{"tls.key": testTLSKeyPEM},
		})
	}
	return objs
}

func brokenSchemeClient() client.Client {
	return fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
}

func writeBlockingPath(t *testing.T, path string) {
	assert.NoError(t, os.WriteFile(path, []byte("not a directory"), 0o644))
}

func newClusterClient(extra ...client.Object) client.Client {
	objs := clusterFixtureObjects()
	objs = append(objs, extra...)
	return fake.NewClientBuilder().WithScheme(testScheme).WithObjects(objs...).Build()
}

func clusterClientExcluding(exclude func(client.Object) bool, extra ...client.Object) client.Client {
	objs := clusterFixtureObjects()
	filtered := make([]client.Object, 0, len(objs))
	for _, obj := range objs {
		if !exclude(obj) {
			filtered = append(filtered, obj)
		}
	}
	filtered = append(filtered, extra...)
	return fake.NewClientBuilder().WithScheme(testScheme).WithObjects(filtered...).Build()
}

type testEnv struct {
	t           *testing.T
	ctrl        *gomock.Controller
	mockOps     *ops.MockOps
	mockOstree  *rpmostreeclient.MockIClient
	backupDir   string
	paths       Paths
	seedCreator *SeedCreator
}

func newTestEnv(t *testing.T, client client.Client, opts ...func(*Options)) *testEnv {
	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	mockOstree := rpmostreeclient.NewMockIClient(ctrl)

	root := t.TempDir()
	backupDir := filepath.Join(root, "backup")
	paths := DefaultPaths()
	paths.BackupChecksDir = filepath.Join(root, "checks")
	paths.InstallationConfigFilesDir = filepath.Join(root, "install-config-files")
	paths.SeedDataDir = filepath.Join(root, "seed-data")
	paths.LCABinarySource = filepath.Join(root, "lca-cli-bin")
	paths.LCABinaryDest = filepath.Join(root, "lca-cli-dest")
	paths.MCDCurrentConfig = filepath.Join(root, "mco-currentconfig.json")
	paths.SystemdUnitDestDir = filepath.Join(root, "systemd")
	paths.DockerfileTempDir = root
	paths.BackupCertsDir = filepath.Join(root, "backup-certs")
	paths.OvnCertDirs = []string{filepath.Join(root, "ovn-certs"), filepath.Join(root, "multus-certs")}

	assert.NoError(t, os.MkdirAll(paths.SystemdUnitDestDir, 0o700))
	assert.NoError(t, os.MkdirAll(backupDir, 0o700))
	assert.NoError(t, os.WriteFile(paths.LCABinarySource, []byte("binary"), 0o700))

	assert.NoError(t, os.WriteFile(paths.MCDCurrentConfig, testMCOConfigData, 0o600))

	options := Options{
		Client:               client,
		Log:                  testLogger(),
		Ops:                  mockOps,
		OstreeClient:         mockOstree,
		BackupDir:            backupDir,
		Paths:                paths,
		ContainerRegistry:    "registry.example/seed:latest",
		AuthFile:             filepath.Join(root, "auth.json"),
		RecertContainerImage: "quay.io/edge-infrastructure/recert:v0",
		RecertSkipValidation: true,
	}
	for _, opt := range opts {
		opt(&options)
	}

	seedCreator := NewSeedCreator(options)
	return &testEnv{
		t:           t,
		ctrl:        ctrl,
		mockOps:     mockOps,
		mockOstree:  mockOstree,
		backupDir:   seedCreator.backupDir,
		paths:       seedCreator.paths,
		seedCreator: seedCreator,
	}
}

func TestGetSeedAdditionalTrustBundleState(t *testing.T) {
	ctx := context.Background()

	t.Run("no user CA bundle", func(t *testing.T) {
		cl := newClusterClient()
		result, err := GetSeedAdditionalTrustBundleState(ctx, cl)
		assert.NoError(t, err)
		assert.False(t, result.HasUserCaBundle)
		assert.Empty(t, result.ProxyConfigmapName)
	})

	t.Run("user CA bundle present", func(t *testing.T) {
		cl := newClusterClient(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: common.ClusterAdditionalTrustBundleName, Namespace: common.OpenshiftConfigNamespace},
			Data:       map[string]string{common.CaBundleDataKey: "bundle-data"},
		})
		result, err := GetSeedAdditionalTrustBundleState(ctx, cl)
		assert.NoError(t, err)
		assert.True(t, result.HasUserCaBundle)
	})

	t.Run("proxy get failure", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(testScheme).Build()
		_, err := GetSeedAdditionalTrustBundleState(ctx, cl)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to get cluster additional trust bundle state")
	})
}

func TestGetCsvRelatedImages(t *testing.T) {
	ctx := context.Background()

	t.Run("filters excluded related image patterns", func(t *testing.T) {
		cl := newClusterClient(
			&operatorsv1alpha1.ClusterServiceVersion{
				ObjectMeta: metav1.ObjectMeta{Name: "test-csv", Namespace: "openshift-operators"},
				Spec: operatorsv1alpha1.ClusterServiceVersionSpec{
					RelatedImages: []operatorsv1alpha1.RelatedImage{
						{Image: "quay.io/valid/app:v1"},
						{Image: "quay.io/valid/app-for-gcp:v1"},
						{Image: "quay.io/valid/app-for-microsoft-azure:v1"},
						{Image: "quay.io/valid/app-for-csi:v1"},
					},
				},
			},
		)
		env := newTestEnv(t, cl)
		images, err := env.seedCreator.getCsvRelatedImages(ctx)
		assert.NoError(t, err)
		assert.Equal(t, []string{"quay.io/valid/app:v1"}, images)
	})

	t.Run("list failure", func(t *testing.T) {
		env := newTestEnv(t, brokenSchemeClient())
		_, err := env.seedCreator.getCsvRelatedImages(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to get csv list")
	})
}

func TestFilterCatalogImages(t *testing.T) {
	ctx := context.Background()

	t.Run("filters catalog source and default index images", func(t *testing.T) {
		cl := newClusterClient(&operatorsv1alpha1.CatalogSource{
			ObjectMeta: metav1.ObjectMeta{Name: "cs", Namespace: "openshift-marketplace"},
			Spec:       operatorsv1alpha1.CatalogSourceSpec{Image: "registry.example/catalog:v1"},
		})
		env := newTestEnv(t, cl)
		filtered, err := env.seedCreator.filterCatalogImages(ctx, []string{
			"registry.example/catalog:v1",
			"registry.redhat.io/redhat/redhat-operator-index:v4.18",
			"quay.io/myapp:v1",
			"",
		})
		assert.NoError(t, err)
		assert.Equal(t, []string{"quay.io/myapp:v1", ""}, filtered)
	})

	t.Run("list failure", func(t *testing.T) {
		env := newTestEnv(t, brokenSchemeClient())
		_, err := env.seedCreator.filterCatalogImages(ctx, []string{"img"})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to list all catalogueSources")
	})
}

func TestGatherClusterInfo(t *testing.T) {
	ctx := context.Background()

	t.Run("happy path", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().GetContainerStorageTarget().Return("/var/lib/containers", nil)

		assert.NoError(t, env.seedCreator.gatherClusterInfo(ctx))
		assert.FileExists(t, filepath.Join(env.paths.SeedDataDir, common.SeedClusterInfoFileName))
		assert.FileExists(t, filepath.Join(env.backupDir, common.SeedClusterInfoFileName))
	})

	t.Run("missing cluster version", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(testScheme).Build()
		env := newTestEnv(t, cl)
		err := env.seedCreator.gatherClusterInfo(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to get cluster info")
	})

	t.Run("container storage target error", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().GetContainerStorageTarget().Return("", errors.New("mount error"))
		err := env.seedCreator.gatherClusterInfo(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to get container storage mountpoint target")
	})

	t.Run("proxy get failure", func(t *testing.T) {
		cl := clusterClientExcluding(func(obj client.Object) bool {
			_, ok := obj.(*configv1.Proxy)
			return ok
		})
		env := newTestEnv(t, cl)
		err := env.seedCreator.gatherClusterInfo(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to get proxy information")
	})

	t.Run("fips lookup failure", func(t *testing.T) {
		cl := clusterClientExcluding(func(obj client.Object) bool {
			_, ok := obj.(*mcv1.MachineConfig)
			return ok
		})
		env := newTestEnv(t, cl)
		err := env.seedCreator.gatherClusterInfo(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to get FIPS information")
	})

	t.Run("seed data dir creation failure", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().GetContainerStorageTarget().Return("/var/lib/containers", nil)
		writeBlockingPath(t, env.paths.SeedDataDir)
		err := env.seedCreator.gatherClusterInfo(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "error creating SeedDataDir")
	})

	t.Run("backup copy failure", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().GetContainerStorageTarget().Return("/var/lib/containers", nil)
		backupFile := filepath.Join(t.TempDir(), "backup-file")
		writeBlockingPath(t, backupFile)
		env.seedCreator.backupDir = backupFile
		err := env.seedCreator.gatherClusterInfo(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "error copying from")
	})
}

func TestHandleServices(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		setupServiceFixture(env, "lca-init-monitor.service", "[Unit]\nDescription=test\n")
		env.mockOps.EXPECT().SystemctlAction("enable", "lca-init-monitor.service").Return("", nil)
		assert.NoError(t, env.seedCreator.handleServices())
		assert.FileExists(t, filepath.Join(env.paths.SystemdUnitDestDir, "lca-init-monitor.service"))
	})

	t.Run("systemctl failure", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		setupServiceFixture(env, "foo.service", "unit")
		env.mockOps.EXPECT().SystemctlAction("enable", "foo.service").Return("", errors.New("systemctl failed"))
		err := env.seedCreator.handleServices()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to enabling service foo.service")
	})

	t.Run("missing services directory", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		err := env.seedCreator.handleServices()
		assert.Error(t, err)
	})

	t.Run("copy failure", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		setupServiceFixture(env, "foo.service", "unit")

		readOnlyParent := filepath.Join(t.TempDir(), "readonly")
		assert.NoError(t, os.Mkdir(readOnlyParent, 0o555))
		env.seedCreator.paths.SystemdUnitDestDir = readOnlyParent

		err := env.seedCreator.handleServices()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed create service foo.service")
	})
}

func TestCreateContainerList(t *testing.T) {
	ctx := context.Background()

	t.Run("happy path", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().RunBashInHostNamespace("podman", "image", "prune", "-f").Return("", nil)
		env.mockOps.EXPECT().RunBashInHostNamespace("crictl", gomock.Any()).Return("quay.io/app:v1\n", nil)

		assert.NoError(t, env.seedCreator.createContainerList(ctx))
		content, err := os.ReadFile(filepath.Join(env.backupDir, common.ContainersListFileName))
		assert.NoError(t, err)
		lines := strings.Split(strings.TrimSpace(string(content)), "\n")
		assert.Contains(t, lines, "quay.io/app:v1")
		assert.Contains(t, lines, env.seedCreator.recertContainerImage)
	})

	t.Run("prune failure", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().RunBashInHostNamespace("podman", "image", "prune", "-f").Return("", errors.New("prune failed"))
		err := env.seedCreator.createContainerList(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to prune with podman")
	})

	t.Run("crictl failure", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().RunBashInHostNamespace("podman", "image", "prune", "-f").Return("", nil)
		env.mockOps.EXPECT().RunBashInHostNamespace("crictl", gomock.Any()).Return("", errors.New("crictl failed"))
		err := env.seedCreator.createContainerList(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to run crictl")
	})

	t.Run("filter catalog images failure", func(t *testing.T) {
		env := newTestEnv(t, brokenSchemeClient())
		env.mockOps.EXPECT().RunBashInHostNamespace("podman", "image", "prune", "-f").Return("", nil)
		env.mockOps.EXPECT().RunBashInHostNamespace("crictl", gomock.Any()).Return("quay.io/app:v1\n", nil)
		err := env.seedCreator.createContainerList(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to filter catalog images")
	})

	t.Run("write failure", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().RunBashInHostNamespace("podman", "image", "prune", "-f").Return("", nil)
		env.mockOps.EXPECT().RunBashInHostNamespace("crictl", gomock.Any()).Return("quay.io/app:v1\n", nil)
		blocking := filepath.Join(t.TempDir(), "blocking")
		writeBlockingPath(t, blocking)
		env.seedCreator.backupDir = blocking
		err := env.seedCreator.createContainerList(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to write container list file")
	})
}

func TestStopServices(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().StopClusterServices().Return(nil)
		assert.NoError(t, env.seedCreator.stopServices())
	})

	t.Run("stop failure", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().StopClusterServices().Return(errors.New("stop failed"))
		err := env.seedCreator.stopServices()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to stop cluster services")
	})
}

func TestBackupVar(t *testing.T) {
	t.Run("happy path includes MCD managed file", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		var captured []string
		env.mockOps.EXPECT().RunBashInHostNamespace("tar", gomock.Any()).DoAndReturn(
			func(_ string, args ...string) (string, error) {
				captured = args
				return "", nil
			},
		)
		assert.NoError(t, env.seedCreator.backupVar())
		assert.Contains(t, captured, "--add-file")
		assert.Contains(t, captured, "/var/lib/kubelet/config.json")
		assert.Contains(t, captured, common.VarFolder)
	})

	t.Run("tar failure", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().RunBashInHostNamespace("tar", gomock.Any()).Return("", errors.New("tar failed"))
		err := env.seedCreator.backupVar()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to run tar for backupVar")
	})

	t.Run("missing MCD config", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.seedCreator.paths.MCDCurrentConfig = filepath.Join(t.TempDir(), "missing.json")
		err := env.seedCreator.backupVar()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unable to get list of MCD managed files")
	})
}

func TestBackupEtc(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		calls := 0
		env.mockOps.EXPECT().RunBashInHostNamespace("ostree", gomock.Any()).Times(2).DoAndReturn(
			func(_ string, args ...string) (string, error) {
				calls++
				if calls == 1 {
					assert.Contains(t, args, "admin")
					assert.Contains(t, args, "config-diff")
				}
				return "", nil
			},
		)
		assert.NoError(t, env.seedCreator.backupEtc())
	})

	t.Run("ostree failure on config-diff", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().RunBashInHostNamespace("ostree", gomock.Any()).Return("", errors.New("ostree failed"))
		err := env.seedCreator.backupEtc()
		assert.Error(t, err)
	})

	t.Run("ostree failure on tar pipeline", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().RunBashInHostNamespace("ostree", gomock.Any()).Return("", nil).Times(1)
		env.mockOps.EXPECT().RunBashInHostNamespace("ostree", gomock.Any()).Return("", errors.New("tar pipeline failed")).Times(1)
		err := env.seedCreator.backupEtc()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed backing up /etc")
	})
}

func TestBackupOstree(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().RunBashInHostNamespace("tar", gomock.Any()).Return("", nil)
		assert.NoError(t, env.seedCreator.backupOstree())
	})

	t.Run("tar failure", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().RunBashInHostNamespace("tar", gomock.Any()).Return("", errors.New("tar failed"))
		assert.Error(t, env.seedCreator.backupOstree())
	})
}

func TestBackupRPMOstree(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().RunBashInHostNamespace("rpm-ostree", gomock.Any()).Return("", nil)
		assert.NoError(t, env.seedCreator.backupRPMOstree())
	})

	t.Run("rpm-ostree failure", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().RunBashInHostNamespace("rpm-ostree", gomock.Any()).Return("", errors.New("rpm-ostree failed"))
		err := env.seedCreator.backupRPMOstree()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to run backup rpmostree")
	})
}

func TestBackupMCOConfig(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().RunBashInHostNamespace("cp", env.paths.MCDCurrentConfig, gomock.Any()).Return("", nil)
		assert.NoError(t, env.seedCreator.backupMCOConfig())
	})

	t.Run("cp failure", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().RunBashInHostNamespace("cp", env.paths.MCDCurrentConfig, gomock.Any()).Return("", errors.New("cp failed"))
		err := env.seedCreator.backupMCOConfig()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to backup MCO config")
	})
}

func TestRemoveOvnCertsFolders(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		certDir := env.paths.OvnCertDirs[0]
		assert.NoError(t, os.MkdirAll(certDir, 0o700))
		assert.NoError(t, os.WriteFile(filepath.Join(certDir, "cert.pem"), []byte("cert"), 0o644))
		assert.NoError(t, env.seedCreator.removeOvnCertsFolders())
		_, err := os.Stat(certDir)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("removal failure", func(t *testing.T) {
		if goruntime.GOOS == "windows" {
			t.Skip("chmod-based removal failure test is unix-specific")
		}
		env := newTestEnv(t, newClusterClient())
		certDir := filepath.Join(t.TempDir(), "ovn-certs")
		assert.NoError(t, os.Mkdir(certDir, 0o755))
		assert.NoError(t, os.WriteFile(filepath.Join(certDir, "cert.pem"), []byte("cert"), 0o644))
		assert.NoError(t, os.Chmod(certDir, 0o000))
		env.seedCreator.paths.OvnCertDirs = []string{certDir}

		err := env.seedCreator.removeOvnCertsFolders()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to remove ovn certs")

		assert.NoError(t, os.Chmod(certDir, 0o700))
	})
}

func TestBackupOstreeOrigin(t *testing.T) {
	status := testOstreeStatus()

	t.Run("happy path", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().RunInHostNamespace("cp", []string{
			"/ostree/deploy/rhcos/deploy/abc123def.origin",
			filepath.Join(env.backupDir, "ostree-abc123def.origin"),
		}).Return("", nil)
		assert.NoError(t, env.seedCreator.backupOstreeOrigin(status))
	})

	t.Run("origin already exists", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		origin := filepath.Join(env.backupDir, "ostree-abc123def.origin")
		assert.NoError(t, os.WriteFile(origin, []byte("exists"), 0o600))
		err := env.seedCreator.backupOstreeOrigin(status)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to get file info")
	})

	t.Run("copy failure", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOps.EXPECT().RunInHostNamespace("cp", gomock.Any()).Return("", errors.New("copy failed"))
		err := env.seedCreator.backupOstreeOrigin(status)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed 'copy' command")
	})
}

func TestCreateAndPushSeedImage(t *testing.T) {
	clusterInfo := `{"cluster_name":"test-cluster"}`
	status := testOstreeStatus()

	t.Run("happy path", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOstree.EXPECT().RpmOstreeVersion().Return(nil, nil)
		env.mockOstree.EXPECT().QueryStatus().Return(status, nil)
		env.mockOps.EXPECT().RunInHostNamespace("cp", gomock.Any()).Return("", nil)
		env.mockOps.EXPECT().RunInHostNamespace("podman", gomock.Any()).DoAndReturn(
			func(_ string, args ...string) (string, error) {
				joined := strings.Join(args, " ")
				assert.Contains(t, joined, common.SeedFormatOCILabel)
				assert.Contains(t, joined, common.SeedClusterInfoOCILabel)
				assert.Contains(t, joined, "build")
				return "", nil
			},
		)
		env.mockOps.EXPECT().RunInHostNamespace("podman", "push", "--authfile", env.seedCreator.authFile, env.seedCreator.containerRegistry).Return("", nil)
		assert.NoError(t, env.seedCreator.createAndPushSeedImage(clusterInfo))
	})

	t.Run("query status failure", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOstree.EXPECT().RpmOstreeVersion().Return(nil, nil)
		env.mockOstree.EXPECT().QueryStatus().Return(nil, errors.New("status failed"))
		err := env.seedCreator.createAndPushSeedImage(clusterInfo)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to query ostree status")
	})

	t.Run("build failure", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOstree.EXPECT().RpmOstreeVersion().Return(nil, nil)
		env.mockOstree.EXPECT().QueryStatus().Return(status, nil)
		env.mockOps.EXPECT().RunInHostNamespace("cp", gomock.Any()).Return("", nil)
		env.mockOps.EXPECT().RunInHostNamespace("podman", gomock.Any()).Return("", errors.New("build failed")).Times(1)
		err := env.seedCreator.createAndPushSeedImage(clusterInfo)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to build seed image")
	})

	t.Run("push failure", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOstree.EXPECT().RpmOstreeVersion().Return(nil, nil)
		env.mockOstree.EXPECT().QueryStatus().Return(status, nil)
		env.mockOps.EXPECT().RunInHostNamespace("cp", gomock.Any()).Return("", nil)
		env.mockOps.EXPECT().RunInHostNamespace("podman", gomock.Any()).Return("", nil).Times(1)
		env.mockOps.EXPECT().RunInHostNamespace("podman", gomock.Any()).Return("", errors.New("authentication required")).Times(1)
		err := env.seedCreator.createAndPushSeedImage(clusterInfo)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to push seed image")
	})

	t.Run("backup origin failure", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOstree.EXPECT().RpmOstreeVersion().Return(nil, nil)
		env.mockOstree.EXPECT().QueryStatus().Return(status, nil)
		env.mockOps.EXPECT().RunInHostNamespace("cp", gomock.Any()).Return("", errors.New("copy failed"))
		err := env.seedCreator.createAndPushSeedImage(clusterInfo)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed 'copy' command")
	})

	t.Run("tempfile creation failure", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		env.mockOstree.EXPECT().RpmOstreeVersion().Return(nil, nil)
		env.mockOstree.EXPECT().QueryStatus().Return(status, nil)
		env.mockOps.EXPECT().RunInHostNamespace("cp", gomock.Any()).Return("", nil)
		blocking := filepath.Join(t.TempDir(), "blocking")
		writeBlockingPath(t, blocking)
		env.seedCreator.paths.DockerfileTempDir = blocking
		err := env.seedCreator.createAndPushSeedImage(clusterInfo)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "error creating temporary file")
	})
}

func setupServiceFixture(env *testEnv, serviceName string, content string) {
	servicesDir := filepath.Join(env.paths.InstallationConfigFilesDir, "services")
	assert.NoError(env.t, os.MkdirAll(servicesDir, 0o700))
	assert.NoError(env.t, os.WriteFile(filepath.Join(servicesDir, serviceName), []byte(content), 0o644))
}

func testOstreeStatus() *rpmostreeclient.Status {
	return &rpmostreeclient.Status{
		Deployments: []rpmostreeclient.Deployment{{ID: "rhcos-abc123def", OSName: "rhcos"}},
	}
}

func expectCreateSeedImageEarlyMocks(env *testEnv) {
	env.mockOps.EXPECT().SystemctlAction(gomock.Any(), gomock.Any()).Return("", nil).AnyTimes()
	env.mockOps.EXPECT().RunBashInHostNamespace("podman", "image", "prune", "-f").Return("", nil)
	env.mockOps.EXPECT().RunBashInHostNamespace("crictl", gomock.Any()).Return("", nil)
	env.mockOps.EXPECT().GetContainerStorageTarget().Return("/var/lib/containers", nil)
}

func expectCreateSeedImageRecertMocks(env *testEnv, hasKubeAdmin bool, recertErr error) {
	call := env.mockOps.EXPECT().ForceExpireSeedCrypto(
		env.seedCreator.recertContainerImage,
		env.seedCreator.authFile,
		hasKubeAdmin,
	)
	if recertErr != nil {
		call.Return(recertErr)
		return
	}
	call.Return(nil)
}

func expectCreateSeedImageMocks(env *testEnv, podmanFn func(args []string) (string, error)) {
	expectCreateSeedImageEarlyMocks(env)
	env.mockOps.EXPECT().StopClusterServices().Return(nil)
	env.mockOps.EXPECT().RunBashInHostNamespace("tar", gomock.Any()).Return("", nil).AnyTimes()
	env.mockOps.EXPECT().RunBashInHostNamespace("ostree", gomock.Any()).Return("", nil).AnyTimes()
	env.mockOps.EXPECT().RunBashInHostNamespace("rpm-ostree", gomock.Any()).Return("", nil)
	env.mockOps.EXPECT().RunBashInHostNamespace("cp", gomock.Any()).Return("", nil)
	env.mockOstree.EXPECT().RpmOstreeVersion().Return(nil, nil)
	env.mockOstree.EXPECT().QueryStatus().Return(testOstreeStatus(), nil)
	env.mockOps.EXPECT().RunInHostNamespace("cp", gomock.Any()).Return("", nil)
	if podmanFn == nil {
		env.mockOps.EXPECT().RunInHostNamespace("podman", gomock.Any()).Return("", nil).AnyTimes()
		return
	}
	env.mockOps.EXPECT().RunInHostNamespace("podman", gomock.Any()).DoAndReturn(
		func(_ string, args ...string) (string, error) {
			return podmanFn(args)
		},
	).AnyTimes()
}

func TestCreateSeedImage(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		setupServiceFixture(env, "foo.service", "unit")
		expectCreateSeedImageMocks(env, nil)

		assert.NoError(t, env.seedCreator.CreateSeedImage())
		assert.FileExists(t, filepath.Join(env.backupDir, common.SeedClusterInfoFileName))
		assert.FileExists(t, filepath.Join(env.backupDir, common.ContainersListFileName))
		assert.FileExists(t, filepath.Join(env.backupDir, "lca-cli"))
	})

	t.Run("fails when services directory is missing", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		err := env.seedCreator.CreateSeedImage()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to add configuration files")
	})

	t.Run("fails when stop services fails", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		setupServiceFixture(env, "foo.service", "unit")
		expectCreateSeedImageEarlyMocks(env)
		env.mockOps.EXPECT().StopClusterServices().Return(errors.New("stop failed"))

		err := env.seedCreator.CreateSeedImage()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to stop services to create seed image")
	})

	t.Run("fails when lca-cli binary copy fails", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		setupServiceFixture(env, "foo.service", "unit")
		env.mockOps.EXPECT().SystemctlAction(gomock.Any(), gomock.Any()).Return("", nil)
		env.seedCreator.paths.LCABinarySource = filepath.Join(t.TempDir(), "missing-binary")
		err := env.seedCreator.CreateSeedImage()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to copy lca-cli binary")
	})

	t.Run("fails when backup dir creation fails", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		setupServiceFixture(env, "foo.service", "unit")
		env.mockOps.EXPECT().SystemctlAction(gomock.Any(), gomock.Any()).Return("", nil)
		blocking := filepath.Join(t.TempDir(), "blocking")
		writeBlockingPath(t, blocking)
		env.seedCreator.backupDir = blocking
		err := env.seedCreator.CreateSeedImage()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to backup dir for seed image")
	})

	t.Run("fails when create_container_list fails", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		setupServiceFixture(env, "foo.service", "unit")
		env.mockOps.EXPECT().SystemctlAction(gomock.Any(), gomock.Any()).Return("", nil)
		env.mockOps.EXPECT().RunBashInHostNamespace("podman", "image", "prune", "-f").Return("", errors.New("prune failed"))
		err := env.seedCreator.CreateSeedImage()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to run once create_container_list")
	})

	t.Run("fails when gather_cluster_info fails", func(t *testing.T) {
		env := newTestEnv(t, fake.NewClientBuilder().WithScheme(testScheme).Build())
		setupServiceFixture(env, "foo.service", "unit")
		env.mockOps.EXPECT().SystemctlAction(gomock.Any(), gomock.Any()).Return("", nil)
		env.mockOps.EXPECT().RunBashInHostNamespace("podman", "image", "prune", "-f").Return("", nil)
		env.mockOps.EXPECT().RunBashInHostNamespace("crictl", gomock.Any()).Return("", nil)
		err := env.seedCreator.CreateSeedImage()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to run once gather_cluster_info")
	})

	t.Run("fails when backup_var fails", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		setupServiceFixture(env, "foo.service", "unit")
		expectCreateSeedImageEarlyMocks(env)
		env.mockOps.EXPECT().StopClusterServices().Return(nil)
		env.mockOps.EXPECT().RunBashInHostNamespace("tar", gomock.Any()).Return("", errors.New("tar failed")).Times(1)
		err := env.seedCreator.CreateSeedImage()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to run once backup_var")
	})

	t.Run("fails when backup_etc fails", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		setupServiceFixture(env, "foo.service", "unit")
		expectCreateSeedImageEarlyMocks(env)
		env.mockOps.EXPECT().StopClusterServices().Return(nil)
		env.mockOps.EXPECT().RunBashInHostNamespace("tar", gomock.Any()).Return("", nil).Times(1)
		env.mockOps.EXPECT().RunBashInHostNamespace("ostree", gomock.Any()).Return("", errors.New("ostree failed")).Times(1)
		err := env.seedCreator.CreateSeedImage()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to run once backup_etc")
	})

	t.Run("fails when remove ovn certs fails", func(t *testing.T) {
		if goruntime.GOOS == "windows" {
			t.Skip("chmod-based removal failure test is unix-specific")
		}
		env := newTestEnv(t, newClusterClient())
		setupServiceFixture(env, "foo.service", "unit")
		expectCreateSeedImageEarlyMocks(env)
		env.mockOps.EXPECT().StopClusterServices().Return(nil)
		for _, certDir := range env.seedCreator.paths.OvnCertDirs {
			assert.NoError(t, os.MkdirAll(certDir, 0o755))
			assert.NoError(t, os.WriteFile(filepath.Join(certDir, "cert.pem"), []byte("cert"), 0o644))
			assert.NoError(t, os.Chmod(certDir, 0o000))
		}
		err := env.seedCreator.CreateSeedImage()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed remove all OVN certs folders")
		for _, certDir := range env.seedCreator.paths.OvnCertDirs {
			assert.NoError(t, os.Chmod(certDir, 0o700))
		}
	})

	t.Run("with recert validation", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "kubeadmin", Namespace: "kube-system"},
			Data:       map[string][]byte{"kubeadmin": []byte("$2a$10$hashed-password")},
		}), func(o *Options) {
			o.RecertSkipValidation = false
		})
		setupServiceFixture(env, "foo.service", "unit")
		expectCreateSeedImageMocks(env, nil)
		expectCreateSeedImageRecertMocks(env, true, nil)

		assert.NoError(t, env.seedCreator.CreateSeedImage())
		assert.DirExists(t, env.paths.BackupCertsDir)
		_, err := os.Stat(filepath.Join(env.paths.BackupCertsDir, "admin-kubeconfig-client-ca.crt"))
		assert.NoError(t, err)
		_, err = os.Stat(filepath.Join(env.paths.BackupChecksDir, "recert.done"))
		assert.NoError(t, err)
	})

	t.Run("fails when recert fails", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient(), func(o *Options) {
			o.RecertSkipValidation = false
		})
		setupServiceFixture(env, "foo.service", "unit")
		expectCreateSeedImageEarlyMocks(env)
		env.mockOps.EXPECT().StopClusterServices().Return(nil)
		expectCreateSeedImageRecertMocks(env, false, errors.New("recert failed"))

		err := env.seedCreator.CreateSeedImage()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to run once recert")
	})

	t.Run("fails when crypto backup fails", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient(), func(o *Options) {
			o.RecertSkipValidation = false
		})
		setupServiceFixture(env, "foo.service", "unit")
		certsFile := filepath.Join(t.TempDir(), "certs-file")
		writeBlockingPath(t, certsFile)
		env.seedCreator.paths.BackupCertsDir = certsFile
		expectCreateSeedImageEarlyMocks(env)

		err := env.seedCreator.CreateSeedImage()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to backing up seed cluster certificates")
	})

	t.Run("fails when push fails", func(t *testing.T) {
		env := newTestEnv(t, newClusterClient())
		setupServiceFixture(env, "foo.service", "unit")
		expectCreateSeedImageMocks(env, func(args []string) (string, error) {
			if len(args) > 0 && args[0] == "push" {
				return "", errors.New("authentication required")
			}
			return "", nil
		})

		err := env.seedCreator.CreateSeedImage()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create and push seed image")
	})
}

func TestDefaultPaths(t *testing.T) {
	paths := DefaultPaths()
	assert.Equal(t, common.BackupChecksDir, paths.BackupChecksDir)
	assert.Equal(t, common.MCDCurrentConfig, paths.MCDCurrentConfig)
	assert.Equal(t, common.LcaCliBinaryHostPath, paths.LCABinaryDest)
	assert.Equal(t, []string{common.OvnNodeCerts, common.MultusCerts}, paths.OvnCertDirs)
}

func TestApplyPathDefaults(t *testing.T) {
	paths := applyPathDefaults(Paths{BackupChecksDir: "/custom/checks"})
	assert.Equal(t, "/custom/checks", paths.BackupChecksDir)
	assert.Equal(t, common.SeedDataDir, paths.SeedDataDir)
	assert.Equal(t, []string{common.OvnNodeCerts, common.MultusCerts}, paths.OvnCertDirs)
}

func TestNewSeedCreatorUsesDefaultBackupDir(t *testing.T) {
	sc := NewSeedCreator(Options{Log: testLogger()})
	assert.Equal(t, common.BackupDir, sc.backupDir)
}
