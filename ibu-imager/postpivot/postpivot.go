package postpivot

import (
	"context"
	"fmt"
	"os"
	"path"
	"time"

	v1 "github.com/openshift/api/config/v1"
	"github.com/sirupsen/logrus"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/clusterinfo"
	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/recert"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

const (
	recertDone = "recert.done"
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

	if err := p.recert(clusterInfo, seedClusterInfo); err != nil {
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
	p.approveCsrs(ctx, client)

	if err := p.applyManifests(); err != nil {
		return err
	}

	if _, err = p.ops.SystemctlAction("disable", "installation-configuration.service"); err != nil {
		return fmt.Errorf("failed to disable installation-configuration.service, err: %w", err)
	}

	p.log.Infof("Removing %s", p.workingDir)
	return os.RemoveAll(p.workingDir)
}

func (p *PostPivot) recert(clusterInfo, seedClusterInfo *clusterinfo.ClusterInfo) error {
	doneFile := path.Join(p.workingDir, recertDone)
	if _, err := os.Stat(doneFile); err == nil {
		p.log.Infof("Found %s file, skippin recert", doneFile)
		return nil
	}

	if _, err := os.Stat(recert.SummaryFile); err == nil {
		return fmt.Errorf("found %s file, returning error, it means recert previously failed. "+
			"In case you still want to rerun it please remove the file", recert.SummaryFile)
	}

	p.log.Info("Create recert configuration file")
	if err := recert.CreateRecertConfigFile(clusterInfo, seedClusterInfo, path.Join(p.workingDir, common.CertsDir),
		p.workingDir); err != nil {
		return err
	}

	if err := p.ops.RunUnauthenticatedEtcdServer(p.authFile, common.EtcdContainerName); err != nil {
		return fmt.Errorf("failed to run etcd, err: %w", err)
	}

	defer func() {
		p.log.Info("Killing the unauthenticated etcd server")
		if _, err := p.ops.RunInHostNamespace("podman", "kill", common.EtcdContainerName); err != nil {
			p.log.WithError(err).Errorf("failed to kill %s container.", common.EtcdContainerName)
		}

		if _, err := p.ops.RunInHostNamespace("podman", "rm", common.EtcdContainerName); err != nil {
			p.log.WithError(err).Errorf("failed to rm %s container.", common.EtcdContainerName)
		}
	}()

	if err := p.additionalCommands(clusterInfo, seedClusterInfo, common.EtcdContainerName); err != nil {
		return err
	}

	if err := p.ops.RunRecert(p.recertContainerImage, p.authFile, path.Join(p.workingDir, recert.RecertConfigFile),
		"-v", fmt.Sprintf("%s:%s", p.workingDir, p.workingDir)); err != nil {
		return err
	}

	_, err := os.Create(doneFile)
	return err
}

func (p *PostPivot) additionalCommands(clusterInfo, seedClusterInfo *clusterinfo.ClusterInfo, etcdImage string) error {
	// TODO: remove after https://issues.redhat.com/browse/ETCD-503
	newEtcdIp := clusterInfo.MasterIP
	if utils.IsIpv6(newEtcdIp) {
		newEtcdIp = fmt.Sprintf("[%s]", newEtcdIp)
	}
	// TODO: move to etcd client?
	_, err := p.ops.RunInHostNamespace("podman", "exec", "-it", etcdImage, "bash", "-c",
		fmt.Sprintf("/usr/bin/etcdctl member list | cut -d',' -f1 | xargs -i etcdctl member update \"{}\" --peer-urls=http://%s:2380", newEtcdIp))
	if err != nil {
		return fmt.Errorf("failed to change etcd peer url, err: %w", err)
	}

	// removing etcd endpoints configmap
	_, err = p.ops.RunInHostNamespace("podman", "exec", "-it", etcdImage, "bash", "-c",
		"/usr/bin/etcdctl del /kubernetes.io/configmaps/openshift-etcd/etcd-endpoints")
	if err != nil {
		return fmt.Errorf("failed to remove etcd-endpoints configmap, err: %w", err)
	}

	// changing seed ip to new ip in all static pod files
	_, err = p.ops.RunInHostNamespace("bash", "-c",
		fmt.Sprintf("find /etc/kubernetes/ -type f -print0 | xargs -0 sed -i \"s/%s/%s/g\"",
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

func (p *PostPivot) approveCsrs(ctx context.Context, client runtimeclient.Client) {
	_ = utils.RunOnce("kube-apiserver-client-kubelet", p.workingDir, p.log, p.approveHostCSR,
		ctx, client, "kube-apiserver-client-kubelet")
	_ = utils.RunOnce("kubelet-serving", p.workingDir, p.log, p.approveHostCSR,
		ctx, client, "kubelet-serving")
}

func (p *PostPivot) approveHostCSR(ctx context.Context, client runtimeclient.Client, signerName string) {
	p.log.Infof("waiting to approve %s csr", signerName)
	_ = wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (done bool, err error) {
		csrList := &certificatesv1.CertificateSigningRequestList{}
		opts := &runtimeclient.ListOptions{FieldSelector: fields.OneTermEqualSelector("spec.signerName",
			fmt.Sprintf("kubernetes.io/%s", signerName))}
		if err = client.List(ctx, csrList, opts); err != nil {
			return false, nil
		}
		oneCSRWasApproved := false

		for _, csr := range csrList.Items {
			if isCsrApproved(&csr) {
				p.log.Infof("Found approved csr %s, skipping it", csr.Name)
				continue
			}
			p.log.Infof("Found not approved csr %s with %s signer name, going to approve it", csr.Name, signerName)
			csr.Status.Conditions = append(csr.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
				Type:           certificatesv1.CertificateApproved,
				Reason:         "NodeCSRApprove",
				Message:        "This CSR was approved by the post pivot operation",
				Status:         corev1.ConditionTrue,
				LastUpdateTime: metav1.Now(),
			})
			err = client.SubResource("approval").Update(ctx, &csr)
			if err != nil {
				p.log.WithError(err).Errorf("Failed to approve CSR %s", csr.Name)
				continue
			}
			p.log.Infof("csr %s with %s signer name, was approved", csr.Name, signerName)
			oneCSRWasApproved = true
		}
		return oneCSRWasApproved, nil
	})

}

func isCsrApproved(csr *certificatesv1.CertificateSigningRequest) bool {
	for _, c := range csr.Status.Conditions {
		if c.Type == certificatesv1.CertificateApproved {
			return true
		}
	}
	return false
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
