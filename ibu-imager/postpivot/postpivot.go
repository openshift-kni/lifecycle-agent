package postpivot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	v1 "github.com/openshift/api/config/v1"
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/sirupsen/logrus"
	"github.com/thoas/go-funk"
	etcdClient "go.etcd.io/etcd/client/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/clusterinfo"
	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/recert"
	"github.com/openshift-kni/lifecycle-agent/utils"
	cp "github.com/otiai10/copy"
)

type PostPivot struct {
	scheme               *runtime.Scheme
	log                  *logrus.Logger
	ops                  ops.Ops
	recertContainerImage string
	authFile             string
	workingDir           string
	kubeconfig           string
}

func NewPostPivot(scheme *runtime.Scheme, log *logrus.Logger, ops ops.Ops,
	recertContainerImage, authFile, workingDir, kubeconfig string) *PostPivot {
	return &PostPivot{
		scheme:               scheme,
		log:                  log,
		ops:                  ops,
		authFile:             authFile,
		recertContainerImage: recertContainerImage,
		workingDir:           workingDir,
		kubeconfig:           kubeconfig,
	}
}

func (p *PostPivot) PostPivotConfiguration(ctx context.Context) error {
	p.log.Info("Reading cluster info")
	clusterInfo, err := clusterinfo.ReadClusterInfoFromFile(
		path.Join(p.workingDir, common.ClusterConfigDir, common.ClusterInfoFileName))
	if err != nil {
		return fmt.Errorf("failed to get cluster info from %s, err: %w", "", err)
	}

	p.log.Info("Reading seed info")
	seedClusterInfo, err := clusterinfo.ReadClusterInfoFromFile(path.Join(p.workingDir, common.SeedManifest))
	if err != nil {
		return fmt.Errorf("failed to get seed info from %s, err: %w", "", err)
	}

	if err := utils.RunOnce("recert", p.workingDir, p.log, p.recert, ctx, clusterInfo, seedClusterInfo); err != nil {
		return err
	}

	if err := p.copyClusterConfigFiles(); err != nil {
		return err
	}

	client, err := utils.CreateKubeClient(p.scheme, p.kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to create k8s client, err: %w", err)
	}

	if _, err := p.ops.SystemctlAction("enable", "kubelet", "--now"); err != nil {
		return err
	}
	p.waitForApi(ctx, client)

	if err := p.deleteAllOldMirrorResources(ctx, client, clusterInfo, seedClusterInfo); err != nil {
		return err
	}

	if err := p.applyManifests(); err != nil {
		return err
	}

	// Restore lvm devices
	if err := utils.RunOnce("recover_lvm_devices", p.workingDir, p.log, p.recoverLvmDevices); err != nil {
		return err
	}

	if _, err = p.ops.SystemctlAction("disable", "installation-configuration.service"); err != nil {
		return fmt.Errorf("failed to disable installation-configuration.service, err: %w", err)
	}

	p.log.Infof("Removing %s", p.workingDir)
	return os.RemoveAll(p.workingDir)
}

func (p *PostPivot) recert(ctx context.Context, clusterInfo, seedClusterInfo *clusterinfo.ClusterInfo) error {
	if _, err := os.Stat(recert.SummaryFile); err == nil {
		return fmt.Errorf("found %s file, returning error, it means recert previously failed. "+
			"In case you still want to rerun it please remove the file", recert.SummaryFile)
	}

	p.log.Info("Create recert configuration file")
	if err := recert.CreateRecertConfigFile(clusterInfo, seedClusterInfo, path.Join(p.workingDir, common.CertsDir),
		p.workingDir); err != nil {
		return err
	}

	additional := func() error {
		return p.additionalCommands(ctx, clusterInfo, seedClusterInfo)
	}

	err := p.ops.RecertFullFlow(p.recertContainerImage, p.authFile, path.Join(p.workingDir, recert.RecertConfigFile),
		nil, additional, "-v", fmt.Sprintf("%s:%s", p.workingDir, p.workingDir))
	return err
}

func (p *PostPivot) etcdPostPivotOperations(ctx context.Context, clusterInfo, seedInfo *clusterinfo.ClusterInfo) error {
	p.log.Info("Start running etcd post pivot operations")
	cli, err := etcdClient.New(etcdClient.Config{
		Endpoints:   []string{common.EtcdDefaultEndpoint},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return err
	}
	defer cli.Close()

	// cleanup etcd keys
	p.log.Info("Cleaning up etcd keys from seed data")
	keysToDelete := []string{"/kubernetes.io/configmaps/openshift-etcd/etcd-endpoints"}
	kvs, err := cli.Get(ctx, "/", etcdClient.WithPrefix())
	if err != nil {
		return err
	}

	for _, kv := range kvs.Kvs {
		if strings.Contains(string(kv.Key), seedInfo.Hostname) ||
			strings.Contains(string(kv.Value), seedInfo.Hostname) ||
			funk.Contains(keysToDelete, string(kv.Key)) {

			p.log.Infof("Deleting etcd key %s", string(kv.Key))
			_, err = cli.Delete(ctx, string(kv.Key))
			if err != nil {
				return err
			}
		}
	}

	newEtcdIp := clusterInfo.MasterIP
	if utils.IsIpv6(newEtcdIp) {
		newEtcdIp = fmt.Sprintf("[%s]", newEtcdIp)
	}

	members, err := cli.MemberList(ctx)
	if err != nil {
		return fmt.Errorf("failed to get etcd members list")
	}
	_, err = cli.MemberUpdate(ctx, members.Members[0].ID, []string{fmt.Sprintf("http://%s:2380", newEtcdIp)})
	if err != nil {
		return fmt.Errorf("failed to change etcd peer url, err: %w", err)
	}

	return nil
}

func (p *PostPivot) additionalCommands(ctx context.Context, clusterInfo, seedClusterInfo *clusterinfo.ClusterInfo) error {
	// TODO: remove after https://issues.redhat.com/browse/ETCD-503
	if err := p.etcdPostPivotOperations(ctx, clusterInfo, seedClusterInfo); err != nil {
		return fmt.Errorf("failed to run post pivot etcd operations, err: %w", err)
	}

	// changing seed ip to new ip in all static pod files
	_, err := p.ops.RunBashInHostNamespace(fmt.Sprintf("find /etc/kubernetes/ -type f -print0 | xargs -0 sed -i \"s/%s/%s/g\"",
		seedClusterInfo.MasterIP, clusterInfo.MasterIP))
	if err != nil {
		return fmt.Errorf("failed to change seed ip to new ip in /etc/kubernetes, err: %w", err)
	}

	return nil
}

func (p *PostPivot) waitForApi(ctx context.Context, client runtimeclient.Client) {
	p.log.Info("Start waiting for api")
	_ = wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (done bool, err error) {
		p.log.Info("waiting for api")
		nodes := &v1.NodeList{}
		if err = client.List(ctx, nodes); err == nil {
			return true, nil
		}
		return false, nil
	})
}

func (p *PostPivot) applyManifests() error {
	p.log.Infof("Applying manifests from %s", path.Join(p.workingDir, common.ClusterConfigDir, common.ManifestsDir))
	args := []string{"--kubeconfig", p.kubeconfig, "apply", "-f"}

	_, err := p.ops.RunInHostNamespace("oc", append(args, path.Join(p.workingDir, common.ClusterConfigDir, common.ManifestsDir))...)
	if err != nil {
		return fmt.Errorf("failed to apply manifests, err: %w", err)
	}

	if _, err := os.Stat(path.Join(p.workingDir, common.ExtraManifestsDir)); err == nil {
		p.log.Infof("Applying extra manifests")
		_, err := p.ops.RunInHostNamespace("oc", append(args, path.Join(p.workingDir, common.ExtraManifestsDir))...)
		if err != nil {
			return fmt.Errorf("failed to apply extra manifests, err: %w", err)
		}
	}
	return nil
}

func (p *PostPivot) recoverLvmDevices() error {
	lvmConfigPath := path.Join(p.workingDir, common.LvmConfigDir)
	lvmDevicesPath := path.Join(lvmConfigPath, path.Base(common.LvmDevicesPath))

	p.log.Infof("Recovering lvm devices from %s", lvmDevicesPath)
	if err := cp.Copy(lvmDevicesPath, common.LvmDevicesPath); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	}

	// Update the online record of PVs and activate all lvm devices in the VGs
	_, err := p.ops.RunInHostNamespace("pvscan", "--cache", "--activate", "ay")
	if err != nil {
		return fmt.Errorf("failed to scan and active lvm devices, err: %w", err)
	}

	return nil
}

func (p *PostPivot) copyClusterConfigFiles() error {
	return utils.CopyFileIfExists(path.Join(p.workingDir, common.ClusterConfigDir, path.Base(common.CABundleFilePath)),
		common.CABundleFilePath)
}

func (p *PostPivot) deleteAllOldMirrorResources(ctx context.Context, client runtimeclient.Client, clusterInfo, seedInfo *clusterinfo.ClusterInfo) error {
	p.log.Info("Deleting ImageContentSourcePolicy and ImageDigestMirrorSet if they exist")
	icsp := &operatorv1alpha1.ImageContentSourcePolicy{}
	if err := client.DeleteAllOf(ctx, icsp); err != nil {
		return fmt.Errorf("failed to delete all icsps %w", err)
	}

	idms := &v1.ImageDigestMirrorSet{}
	if err := client.DeleteAllOf(ctx, idms); err != nil {
		return fmt.Errorf("failed to delete all idms %w", err)
	}

	if err := p.changeRegistryInCSVDeployment(ctx, client, clusterInfo, seedInfo); err != nil {
		return err
	}

	return p.deleteCatalogSources(ctx, client)
}

func (p *PostPivot) deleteCatalogSources(ctx context.Context, client runtimeclient.Client) error {
	p.log.Info("Deleting default catalog sources")
	catalogSources := &operatorsv1alpha1.CatalogSourceList{}
	allNamespaces := runtimeclient.ListOptions{Namespace: metav1.NamespaceAll}
	if err := client.List(ctx, catalogSources, &allNamespaces); err != nil {
		return fmt.Errorf("failed to list all catalogueSources %w", err)
	}

	for _, catalogSource := range catalogSources.Items {
		if err := client.Delete(ctx, &catalogSource); err != nil {
			return fmt.Errorf("failed to delete all catalogueSources %w", err)
		}
	}

	return nil
}

func (p *PostPivot) changeRegistryInCSVDeployment(ctx context.Context, client runtimeclient.Client,
	clusterInfo, seedInfo *clusterinfo.ClusterInfo) error {

	p.log.Info("Changing release registry in csv deployment")
	cmClient := clusterinfo.NewClusterInfoClient(client)
	csvD, err := cmClient.GetCSVDeployment(ctx)
	if err != nil {
		return err
	}

	if clusterInfo.ReleaseRegistry == seedInfo.ReleaseRegistry {
		p.log.Infof("No registry change occurred, skipping")
		return nil
	}

	p.log.Infof("Changing release registry from %s to %s", seedInfo.ReleaseRegistry, clusterInfo.ReleaseRegistry)
	newImage, err := utils.ReplaceImageRegistry(csvD.Spec.Template.Spec.Containers[0].Image,
		clusterInfo.ReleaseRegistry, seedInfo.ReleaseRegistry)
	if err != nil {
		return err
	}

	var newArgs []string
	for _, arg := range csvD.Spec.Template.Spec.Containers[0].Args {
		if strings.HasPrefix(arg, "--release-image") {
			arg = fmt.Sprintf("--release-image=%s", newImage)
		}
		newArgs = append(newArgs, arg)
	}
	newArgsStr, err := json.Marshal(newArgs)
	if err != nil {
		return err
	}
	patch := []byte(fmt.Sprintf(`{"spec":{"template":{"spec": {"containers": [{"name": "%s", "image":"%s", "args": %s}]}}}}`,
		csvD.Spec.Template.Spec.Containers[0].Name, newImage, newArgsStr))

	p.log.Infof("Applying csv deploymet patch %s", string(patch))
	err = client.Patch(ctx, csvD, runtimeclient.RawPatch(types.StrategicMergePatchType, patch))
	if err != nil {
		return fmt.Errorf("failed to patch csv deployment, err: %w", err)
	}
	p.log.Infof("Done changing csv deployment")
	return nil
}
