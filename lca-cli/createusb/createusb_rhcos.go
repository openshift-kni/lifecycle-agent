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

package createusb

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	ostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
)

const rhcosBuildsURL = "https://rhcos.mirror.openshift.com/art/storage/prod/streams"

var (
	modernRHCOSBuildID = regexp.MustCompile(`^((?:9|10)\.[0-9]+)\.[0-9]+-[0-9]+$`)
	legacyRHCOSBuildID = regexp.MustCompile(`^4([0-9]{2})\.([0-9])([0-9])\.[0-9]+-[0-9]+$`)
)

func rhcosBuildStream(buildID string) (string, error) {
	if match := modernRHCOSBuildID.FindStringSubmatch(buildID); match != nil {
		return "rhel-" + match[1], nil
	}
	if match := legacyRHCOSBuildID.FindStringSubmatch(buildID); match != nil {
		return fmt.Sprintf("4.%s-%s.%s", match[1], match[2], match[3]), nil
	}
	return "", fmt.Errorf("unsupported RHCOS build ID %q; provide an explicit live ISO URL and SHA-256 checksum", buildID)
}

type seedRHCOSVersionReader interface {
	ReadVersion(seedImage, authFile string) (string, error)
}

type opsSeedRHCOSVersionReader struct{ ops ops.Ops }

func (r *opsSeedRHCOSVersionReader) ReadVersion(seedImage, authFile string) (string, error) {
	var version string
	err := withMountedImage(r.ops, seedImage, authFile, func(mountpoint string) error {
		data, err := r.ops.ReadFile(filepath.Join(mountpoint, "rpm-ostree.json"))
		if err != nil {
			return fmt.Errorf("reading seed rpm-ostree.json: %w", err)
		}
		var status ostreeclient.Status
		if err := json.Unmarshal(data, &status); err != nil {
			return fmt.Errorf("decoding seed rpm-ostree.json: %w", err)
		}
		for _, deployment := range status.Deployments {
			if deployment.Booted {
				version = deployment.Version
				return nil
			}
		}
		return fmt.Errorf("seed rpm-ostree.json has no booted deployment")
	})
	return version, err
}

func resolveSeedLiveISO(ctx context.Context, fetcher isoFetcher, reader seedRHCOSVersionReader, seedImage, authFile, workDir string) (RHCOSLiveISO, error) {
	if reader == nil {
		return RHCOSLiveISO{}, fmt.Errorf("seed RHCOS version reader is unavailable")
	}
	buildID, err := reader.ReadVersion(seedImage, authFile)
	if err != nil {
		return RHCOSLiveISO{}, fmt.Errorf("resolving RHCOS live ISO from seed: %w", err)
	}
	stream, err := rhcosBuildStream(buildID)
	if err != nil {
		return RHCOSLiveISO{}, err
	}
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64", "ppc64le": "ppc64le", "s390x": "s390x"}[runtime.GOARCH]
	if arch == "" {
		return RHCOSLiveISO{}, fmt.Errorf("unsupported RHCOS architecture %q", runtime.GOARCH)
	}
	baseURL := fmt.Sprintf("%s/%s/builds/%s/%s/", rhcosBuildsURL, stream, buildID, arch)
	metadataPath := filepath.Join(workDir, "rhcos-meta.json")
	defer func() { _ = os.Remove(metadataPath) }()
	if err := fetcher.FetchToFile(ctx, baseURL+"meta.json", metadataPath); err != nil {
		return RHCOSLiveISO{}, fmt.Errorf("fetching metadata for seed RHCOS build %s: %w", buildID, err)
	}
	data, err := os.ReadFile(metadataPath) //nolint:gosec // fixed metadata filename in the build work directory
	if err != nil {
		return RHCOSLiveISO{}, fmt.Errorf("reading RHCOS build metadata: %w", err)
	}
	var metadata struct {
		BuildID string `json:"buildid"`
		Images  map[string]struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"images"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return RHCOSLiveISO{}, fmt.Errorf("decoding RHCOS build metadata: %w", err)
	}
	artifact := metadata.Images["live-iso"]
	if metadata.BuildID != buildID || artifact.Path == "" || path.Base(artifact.Path) != artifact.Path || strings.Contains(artifact.Path, "\\") || !strings.HasSuffix(artifact.Path, ".iso") || !sha256HexRegex.MatchString(artifact.SHA256) {
		return RHCOSLiveISO{}, fmt.Errorf("RHCOS metadata does not describe a valid live ISO for seed build %s", buildID)
	}
	return RHCOSLiveISO{URL: baseURL + artifact.Path, SHA256: artifact.SHA256}, nil
}
