package imagemgmt

import (
	"encoding/json"
	"fmt"
	"sort"
	"syscall"

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"github.com/samber/lo"

	lcautils "github.com/openshift-kni/lifecycle-agent/utils"
)

// ImageMgmtIntf is an interface for LCA iamge management commands.
//
//go:generate mockgen -source=imagemgmt.go -package=imagemgmt -destination=mock_imagemgmt.go
type ImageMgmtIntf interface {
	CheckDiskUsageAgainstThreshold(thresholdPercent int) (bool, error)
	GetInuseImages() (inUse []string, rc error)
	GetPinnedImages() (pinned []string, rc error)
	GetRemovalCandidates() (removalCandidates []imageMgmtImageInfo, rc error)
	CleanupUnusedImages(thresholdPercent int) error
}

type ImageMgmtClient struct {
	Log         *logr.Logger
	Executor    ops.Execute
	StoragePath string
}

// NewImageMgmtClient creates and returns a ImageMgmtIntf interface for image management commands
func NewImageMgmtClient(log *logr.Logger, hostCommandsExecutor ops.Execute, storagePath string) ImageMgmtIntf {
	return &ImageMgmtClient{
		Log:         log,
		Executor:    hostCommandsExecutor,
		StoragePath: storagePath,
	}
}

// imageMgmtImageInfo struct provides the image data required for image management
type imageMgmtImageInfo struct {
	Id          string
	RepoTags    []string
	RepoDigests []string
	Names       []string
	Created     uint64
	Dangling    bool
}

// Types for image data from crictl commands
type crictlContainerImageData struct {
	Image string `json:"image"`
}
type crictlContainerData struct {
	Image crictlContainerImageData `json:"image"`
}

type crictlContainersList struct {
	Containers []crictlContainerData `json:"containers"`
}

type crictlImageData struct {
	Id     string `json:"id"`
	Pinned bool   `json:"pinned"`
}

type crictlImagesList struct {
	Images []crictlImageData `json:"images"`
}

// Types for image data from podman commands
type podmanContainerData struct {
	ImageID string
}

// Use a var for syscall.Statfs in order to override it in mock tests
var syscallStatfs = syscall.Statfs

// CheckDiskUsageAgainstThreshold gets the disk usage for the specified path and checks whether it exceeds the specified threshold
func (c *ImageMgmtClient) CheckDiskUsageAgainstThreshold(thresholdPercent int) (bool, error) {
	var stat syscall.Statfs_t
	if err := syscallStatfs(c.StoragePath, &stat); err != nil {
		return false, fmt.Errorf("statfs failed for %s: %w", c.StoragePath, err)
	}

	// Get disk usage percentage
	// nolint: gosec
	usedpct := int(((stat.Blocks - stat.Bavail) * 100) / stat.Blocks)
	c.Log.Info(fmt.Sprintf("DEBUG: Disk usage of %s is currently %d%%", c.StoragePath, usedpct))
	return (usedpct > thresholdPercent), nil
}

// GetInuseImages gets the list of images in use by cri-o or podman containers
func (c *ImageMgmtClient) GetInuseImages() (inUse []string, rc error) {
	// Get the list of containers via crictl
	output, err := c.Executor.Execute("crictl", "ps", "-a", "-o", "json")
	if err != nil {
		rc = fmt.Errorf("failed to run crictl ps: %w", err)
		return
	}

	var crictlContainers crictlContainersList
	if err := json.Unmarshal([]byte(output), &crictlContainers); err != nil {
		rc = fmt.Errorf("unable to parse crictl ps command output: %w", err)
		return
	}

	for _, container := range crictlContainers.Containers {
		inUse = lcautils.AppendToListIfNotExists(inUse, container.Image.Image)
	}

	// Get the list of containers via podman
	output, err = c.Executor.Execute("podman", "ps", "-a", "--format", "json", "--log-level", "error")
	if err != nil {
		rc = fmt.Errorf("failed to run podman ps: %w", err)
		return
	}

	var podmanContainers []podmanContainerData
	if err := json.Unmarshal([]byte(output), &podmanContainers); err != nil {
		rc = fmt.Errorf("unable to parse podman ps command output: %w", err)
		return
	}

	for _, container := range podmanContainers {
		inUse = lcautils.AppendToListIfNotExists(inUse, container.ImageID)
	}

	return
}

// GetPinnedImages gets the list of pinned images, from crictl
func (c *ImageMgmtClient) GetPinnedImages() (pinned []string, rc error) {
	// Get the list of containers via crictl
	output, err := c.Executor.Execute("crictl", "images", "-o", "json")
	if err != nil {
		rc = fmt.Errorf("failed to run crictl images: %w", err)
		return
	}

	var crictlImages crictlImagesList
	if err := json.Unmarshal([]byte(output), &crictlImages); err != nil {
		rc = fmt.Errorf("unable to parse crictl images command output: %w", err)
		return
	}

	for _, image := range crictlImages.Images {
		if image.Pinned {
			pinned = lcautils.AppendToListIfNotExists(pinned, image.Id)
		}
	}

	return
}

// isImageUsed compares the specified image info against the list of in-use images to check whether the image ID, tag, or digest is in the in-use list
func isImageUsed(inUse []string, image imageMgmtImageInfo) bool {
	if lo.Contains(inUse, image.Id) {
		return true
	}

	for _, ref := range image.RepoTags {
		if lo.Contains(inUse, ref) {
			return true
		}
	}

	for _, ref := range image.RepoDigests {
		if lo.Contains(inUse, ref) {
			return true
		}
	}

	for _, ref := range image.Names {
		if lo.Contains(inUse, ref) {
			return true
		}
	}

	return false
}

// GetRemovalCandidates gets the list of images sorted by creation timestamp, filtering out in-use and pinned images
func (c *ImageMgmtClient) GetRemovalCandidates() (removalCandidates []imageMgmtImageInfo, rc error) {
	inUse, err := c.GetInuseImages()
	if err != nil {
		rc = fmt.Errorf("failure getting list of in-use images: %w", err)
		return
	}

	pinned, err := c.GetPinnedImages()
	if err != nil {
		rc = fmt.Errorf("failure getting list of pinned images: %w", err)
		return
	}

	// Get a list of candidate images for removal, excluding pinned and in-use images

	var images []imageMgmtImageInfo

	output, err := c.Executor.Execute("podman", "images", "--format", "json", "--log-level", "error")
	if err != nil {
		rc = fmt.Errorf("failed to run podman images command: %w", err)
		return
	}

	if err := json.Unmarshal([]byte(output), &images); err != nil {
		rc = fmt.Errorf("unable to parse podman images command output: %w", err)
		return
	}

	// Sort images by age, with dangling images first
	sort.SliceStable(images, func(i, j int) bool {
		// If Dangling bool matches, sort by Created
		if images[i].Dangling == images[j].Dangling {
			return images[i].Created < images[j].Created
		}
		// Dangling images first
		return images[i].Dangling
	})

	// Filter list of images to get candidates for removal
	for _, image := range images {
		// Skip in-use or pinned images
		if lo.Contains(pinned, image.Id) || isImageUsed(inUse, image) {
			continue
		}

		removalCandidates = append(removalCandidates, image)
	}

	return
}

// CleanupUnusedImages iterates through the image removal candidates,
// deleting in sets of 5 until the container storage disk usage threshold is met
func (c *ImageMgmtClient) CleanupUnusedImages(thresholdPercent int) error {
	removalCandidates, err := c.GetRemovalCandidates()
	if err != nil {
		return fmt.Errorf("failed to get removal candidates: %w", err)
	}

	// Remove images in sets of 5 until threshold is met
	totalCandidates := len(removalCandidates)
	var toRemove []string
	for idx, image := range removalCandidates {
		toRemove = append(toRemove, image.Id)
		toRemove = append(toRemove, image.RepoTags...)
		toRemove = append(toRemove, image.RepoDigests...)

		if idx%5 == 4 || idx == totalCandidates-1 {
			// Delete images
			args := []string{"rmi"}
			args = append(args, toRemove...)

			// Ignore errors, as images may be in use by transient containers or other images
			output, _ := c.Executor.Execute("podman", args...)
			c.Log.Info("Deleted unused images", "output", output)

			// Check container storage disk usage
			if exceeded, err := c.CheckDiskUsageAgainstThreshold(thresholdPercent); err != nil {
				return fmt.Errorf("failed to check container storage disk usage: %w", err)
			} else if !exceeded {
				// Container storage disk usage is within threshold
				c.Log.Info("Container storage disk usage is now within threshold", "thresholdPercent", thresholdPercent)
				return nil
			}

			// Clear list
			toRemove = []string{}
		}
	}

	// If we've gotten here, we haven't cleaned enough space. Log a warning, but allow Prep to continue
	c.Log.Info("WARNING: Unable to clean enough images to get container storage usage under threshold")
	return nil
}
