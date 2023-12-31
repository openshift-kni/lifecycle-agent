package ibi_preparation

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
	"github.com/sirupsen/logrus"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/ibu-imager/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/precache"
	"github.com/openshift-kni/lifecycle-agent/internal/precache/workload"
	"github.com/openshift-kni/lifecycle-agent/internal/prep"
)

type IBIPrepare struct {
	log                 *logrus.Logger
	ops                 ops.Ops
	authFile            string
	seedImage           string
	rpmostreeClient     rpmostreeclient.IClient
	ostreeClient        ostreeclient.IClient
	seedExpectedVersion string
	pullSecretFile      string
}

func NewIBIPrepare(log *logrus.Logger, ops ops.Ops, rpmostreeClient rpmostreeclient.IClient,
	ostreeClient ostreeclient.IClient, seedImage, authFile, pullSecretFile, seedExpectedVersion string) *IBIPrepare {
	return &IBIPrepare{
		log:                 log,
		ops:                 ops,
		authFile:            authFile,
		pullSecretFile:      pullSecretFile,
		seedImage:           seedImage,
		rpmostreeClient:     rpmostreeClient,
		ostreeClient:        ostreeClient,
		seedExpectedVersion: seedExpectedVersion,
	}
}

func (i *IBIPrepare) Run() error {
	var imageList []string
	imageListFile := "var/tmp/imageListFile"

	// Pull seed image
	i.log.Info("Pulling seed image")
	if _, err := i.ops.RunInHostNamespace("podman", "pull", "--authfile", i.authFile, i.seedImage); err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}

	// TODO: change to logrus after refactoring the code in controllers and moving to logrus
	log := logr.Logger{}
	common.OstreeDeployPathPrefix = "/mnt/"
	// Setup state root
	if err := prep.SetupStateroot(log, i.ops, i.ostreeClient, i.rpmostreeClient,
		i.seedImage, i.seedExpectedVersion, imageListFile, true); err != nil {
		return err
	}

	// TODO: add support for mirror registry
	imageList, err := prep.ReadPrecachingList(imageListFile, "", "", false)
	if err != nil {
		err = fmt.Errorf("failed to read pre-caching image file: %s, %w", common.PathOutsideChroot(imageListFile), err)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(precache.StatusFile), 0o700); err != nil {
		return fmt.Errorf("failed to create status file dir, err %w", err)
	}

	report, err := workload.PullImages(imageList, i.pullSecretFile)
	if err != nil {
		return err
	}
	i.log.Infof("Precaching report %v", report)

	return nil
}
