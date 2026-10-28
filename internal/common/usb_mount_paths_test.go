package common

import (
	"path/filepath"
	"strings"
	"testing"
)

// On the RHCOS live ISO, /mnt is a symlink to /var/mnt. The installer mounts
// the target root there, hiding any earlier USB mounts under that directory.
func TestUSBMountPathsStayVisibleAfterTargetRootMount(t *testing.T) {
	for _, path := range []string{LocalImagesMountPath, IBIStatusMountPath} {
		if !filepath.IsAbs(path) || path == "/mnt" || path == "/var/mnt" ||
			strings.HasPrefix(path, "/mnt/") || strings.HasPrefix(path, "/var/mnt/") {
			t.Errorf("USB mount path %q would be hidden by the target root mounted at /mnt", path)
		}
	}
}
