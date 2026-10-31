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
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeSeedVersion struct {
	version string
	err     error
}

func (f fakeSeedVersion) ReadVersion(_, _ string) (string, error) { return f.version, f.err }

type metadataFetcher struct{ content string }

func (f metadataFetcher) FetchToFile(_ context.Context, _, destination string) error {
	return os.WriteFile(destination, []byte(f.content), 0o600)
}

func TestResolveSeedLiveISO(t *testing.T) {
	const build = "9.8.20260922-1"
	const sha = "82de39b0a68c4a1ba354a7cfdc16988619c0e3b590101127d73470872bd1c650"
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64", "ppc64le": "ppc64le", "s390x": "s390x"}[runtime.GOARCH]
	artifact := "rhcos-" + build + "-live-iso." + arch + ".iso"
	meta := fmt.Sprintf(`{"buildid":%q,"images":{"live-iso":{"path":%q,"sha256":%q}}}`, build, artifact, sha)
	iso, err := resolveSeedLiveISO(context.Background(), metadataFetcher{meta}, fakeSeedVersion{version: build}, "seed", "", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, sha, iso.SHA256)
	require.Equal(t, "https://rhcos.mirror.openshift.com/art/storage/prod/streams/rhel-9.8/builds/"+build+"/"+arch+"/"+artifact, iso.URL)

	const rhel10Build = "10.2.20260715-0"
	rhel10Artifact := "rhcos-" + rhel10Build + "-live-iso." + arch + ".iso"
	rhel10Meta := fmt.Sprintf(`{"buildid":%q,"images":{"live-iso":{"path":%q,"sha256":%q}}}`, rhel10Build, rhel10Artifact, sha)
	rhel10ISO, err := resolveSeedLiveISO(context.Background(), metadataFetcher{rhel10Meta}, fakeSeedVersion{version: rhel10Build}, "seed", "", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, "https://rhcos.mirror.openshift.com/art/storage/prod/streams/rhel-10.2/builds/"+rhel10Build+"/"+arch+"/"+rhel10Artifact, rhel10ISO.URL)

	const legacyBuild = "418.94.202511191518-0"
	legacyArtifact := "rhcos-" + legacyBuild + "-live." + arch + ".iso"
	legacyMeta := fmt.Sprintf(`{"buildid":%q,"images":{"live-iso":{"path":%q,"sha256":%q}}}`, legacyBuild, legacyArtifact, sha)
	legacyISO, err := resolveSeedLiveISO(context.Background(), metadataFetcher{legacyMeta}, fakeSeedVersion{version: legacyBuild}, "seed", "", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, "https://rhcos.mirror.openshift.com/art/storage/prod/streams/4.18-9.4/builds/"+legacyBuild+"/"+arch+"/"+legacyArtifact, legacyISO.URL)

	for _, tc := range []struct{ name, version, metadata, message string }{
		{"unsupported seed build", "4.22.16", meta, "unsupported RHCOS build ID"},
		{"wrong metadata build", build, strings.Replace(meta, build, "9.8.20260923-1", 1), "does not describe"},
		{"unsafe artifact path", build, strings.Replace(meta, artifact, "../other.iso", 1), "does not describe"},
		{"missing checksum", build, strings.Replace(meta, sha, "", 1), "does not describe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveSeedLiveISO(context.Background(), metadataFetcher{tc.metadata}, fakeSeedVersion{version: tc.version}, "seed", "", t.TempDir())
			require.ErrorContains(t, err, tc.message)
		})
	}
}
