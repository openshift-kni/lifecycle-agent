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
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "gopkg.in/yaml.v3"

	"github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

// writeFile writes content under dir/name (creating parents) and returns the path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// ----- RenderP3Tree -----

func TestRenderP3Tree_SeedReconfiguration(t *testing.T) {
	m := validSelfContained()
	m.SiteConfig.NodeIPs = []string{"192.0.2.10"}
	m.SiteConfig.MachineNetworks = []string{"192.0.2.0/24"}
	m.SiteConfig.RawNMStateConfig = "interfaces: []"
	m.SiteConfig.SSHKey = "ssh-ed25519 AAAA..."
	m.ClusterManifests = nil // focus on the manifest.json render

	p3Root := t.TempDir()
	result, err := RenderP3Tree(m, p3Root)
	require.NoError(t, err)
	assert.Equal(t, p3Root, result.Root)

	manifestPath := filepath.Join(p3Root, common.ClusterConfigDir, common.SeedReconfigurationFileName)
	assert.FileExists(t, manifestPath)

	sr, err := utils.ReadSeedReconfigurationFromFile(manifestPath)
	require.NoError(t, err)
	// api_version must be the only version post-pivot accepts.
	assert.Equal(t, seedreconfig.SeedReconfigurationVersion, sr.APIVersion)
	assert.Equal(t, "example", sr.ClusterName)
	assert.Equal(t, "example.com", sr.BaseDomain)
	assert.Equal(t, "node0", sr.Hostname)
	assert.Equal(t, []string{"192.0.2.10"}, sr.NodeIPs)
	assert.Equal(t, []string{"192.0.2.0/24"}, sr.MachineNetworks)
	assert.Equal(t, "interfaces: []", sr.RawNMStateConfig)
	assert.Equal(t, "ssh-ed25519 AAAA...", sr.SSHKey)
	assert.Equal(t, validPullSecret, sr.PullSecret)
}

func TestRenderP3Tree_InstallOnlyRejected(t *testing.T) {
	_, err := RenderP3Tree(validInstallOnly(), t.TempDir())
	assert.ErrorContains(t, err, "only rendered for mode")
}

func TestRenderP3Tree_ManifestExpansion(t *testing.T) {
	srcDir := t.TempDir()
	// A directory source with files (lexically b before c), a nested sub-directory that must
	// be ignored (non-recursive), and a standalone file source.
	writeFile(t, srcDir, "cluster/c-second.yaml", "kind: ConfigMap\n")
	writeFile(t, srcDir, "cluster/b-first.yaml", "kind: Secret\n")
	writeFile(t, srcDir, "cluster/nested/deep.yaml", "kind: Ignored\n")
	standalone := writeFile(t, srcDir, "standalone.yaml", "kind: Standalone\n")

	m := validSelfContained()
	m.ClusterManifests = &[]ManifestSource{
		{Path: filepath.Join(srcDir, "cluster")},
		{Path: standalone},
	}
	m.ExtraManifests = nil

	p3Root := t.TempDir()
	result, err := RenderP3Tree(m, p3Root)
	require.NoError(t, err)

	manifestsDir := filepath.Join(p3Root, common.ClusterConfigDir, common.ManifestsDir)
	// Non-recursive lexical expansion: b-first, c-second, then the standalone file. The
	// nested sub-directory is not expanded.
	assert.Equal(t, []string{
		filepath.Join(manifestsDir, "b-first.yaml"),
		filepath.Join(manifestsDir, "c-second.yaml"),
		filepath.Join(manifestsDir, "standalone.yaml"),
	}, result.ManifestPaths)
	assert.NoFileExists(t, filepath.Join(manifestsDir, "deep.yaml"))

	// Files are copied verbatim.
	got, err := os.ReadFile(filepath.Join(manifestsDir, "b-first.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "kind: Secret\n", string(got))
}

func TestRenderP3Tree_ExtraManifestsDir(t *testing.T) {
	srcDir := t.TempDir()
	extra := writeFile(t, srcDir, "extra.yaml", "kind: Extra\n")

	m := validSelfContained()
	m.ClusterManifests = &[]ManifestSource{{Path: writeFile(t, srcDir, "cluster.yaml", "kind: Cluster\n")}}
	m.ExtraManifests = &[]ManifestSource{{Path: extra}}

	p3Root := t.TempDir()
	result, err := RenderP3Tree(m, p3Root)
	require.NoError(t, err)

	assert.FileExists(t, filepath.Join(p3Root, common.ClusterConfigDir, common.ManifestsDir, "cluster.yaml"))
	assert.FileExists(t, filepath.Join(p3Root, common.ExtraManifestsDir, "extra.yaml"))
	assert.Len(t, result.ManifestPaths, 2)
}

func TestRenderP3Tree_CollisionFails(t *testing.T) {
	srcDir := t.TempDir()
	a := writeFile(t, srcDir, "a/dup.yaml", "kind: A\n")
	b := writeFile(t, srcDir, "b/dup.yaml", "kind: B\n")

	m := validSelfContained()
	// Two sources whose files render to the same destination basename (dup.yaml).
	m.ClusterManifests = &[]ManifestSource{{Path: a}, {Path: b}}
	m.ExtraManifests = nil

	_, err := RenderP3Tree(m, t.TempDir())
	assert.ErrorContains(t, err, "destination-name collision")
}

// TestRenderP3Tree_NonemptyDestinationRejected proves a reused, nonempty p3Root is rejected
// so a manifest left over from a previous build cannot ship unscanned and unpinned (a
// removed input would otherwise survive on the media and still be applied by post-pivot).
func TestRenderP3Tree_NonemptyDestinationRejected(t *testing.T) {
	p3Root := t.TempDir()
	// A stale artifact from a previous build.
	writeFile(t, p3Root, "extra-manifests/leftover.yaml", "kind: Leftover\n")

	_, err := RenderP3Tree(validSelfContained(), p3Root)
	assert.ErrorContains(t, err, "is not empty")

	// An empty (or absent) destination renders normally.
	fresh := validSelfContained()
	fresh.ClusterManifests = nil
	_, err = RenderP3Tree(fresh, t.TempDir())
	require.NoError(t, err)
}

// ----- ScanImageReferences -----

func TestScanImageReferences_KnownFields(t *testing.T) {
	dir := t.TempDir()
	// A multi-document file exercising container/initContainer/ephemeralContainer fields and
	// a CatalogSource spec.image, with a duplicate to prove dedup.
	path := writeFile(t, dir, "workloads.yaml", `apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      initContainers:
      - name: init
        image: quay.io/test/init:1.0
      containers:
      - name: main
        image: quay.io/test/main:1.0
      - name: dup
        image: quay.io/test/main:1.0
      ephemeralContainers:
      - name: debug
        image: quay.io/test/debug:1.0
---
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
spec:
  image: quay.io/test/catalog:1.0
`)

	images, err := ScanImageReferences([]string{path})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"quay.io/test/init:1.0",
		"quay.io/test/main:1.0",
		"quay.io/test/debug:1.0",
		"quay.io/test/catalog:1.0",
	}, images)
}

func TestScanImageReferences_Normalization(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "norm.yaml", `kind: Pod
spec:
  containers:
  - name: bare
    image: busybox
  - name: shorttag
    image: busybox:1.32
  - name: untaggedpath
    image: registry.example.com/team/app
  - name: digested
    image: quay.io/test/pinned@sha256:`+validSHA256+`
`)

	images, err := ScanImageReferences([]string{path})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"docker.io/library/busybox:latest",
		"docker.io/library/busybox:1.32",
		"registry.example.com/team/app:latest",
		"quay.io/test/pinned@sha256:" + validSHA256,
	}, images)
}

func TestScanImageReferences_CustomResource(t *testing.T) {
	dir := t.TempDir()
	// A custom resource whose image lives in a non-standard field (collected via the
	// parser-validated fallback) alongside values that must NOT be collected: apiVersion
	// (apps/v1, networking.k8s.io/v1), a plain word, and a URL.
	path := writeFile(t, dir, "cr.yaml", `apiVersion: example.com/v1
kind: MyOperator
spec:
  operandImage: registry.example.com/op/operand:v2
  rulesRef: networking.k8s.io/v1
  mode: enabled
  endpoint: https://example.com/api
`)

	images, err := ScanImageReferences([]string{path})
	require.NoError(t, err)
	assert.Equal(t, []string{"registry.example.com/op/operand:v2"}, images)
}

// TestScanImageReferences_ImageSuffixField proves a short, domain-less image reference in a
// custom field whose key ends in "image" (operandImage: busybox:1.32) is discovered rather
// than silently skipped by the generic fallback, while an unrelated field with the same
// short-ref shape is not collected.
func TestScanImageReferences_ImageSuffixField(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "cr.yaml", `apiVersion: example.com/v1
kind: MyOperator
spec:
  operandImage: busybox:1.32
  kubeRBACProxyImage: quay.io/test/proxy:2.0
  schemaVersion: v1:beta
`)

	images, err := ScanImageReferences([]string{path})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"docker.io/library/busybox:1.32",
		"quay.io/test/proxy:2.0",
	}, images)
}

func TestScanImageReferences_RejectsInvalidImageField(t *testing.T) {
	path := writeFile(t, t.TempDir(), "invalid.yaml", `kind: Pod
spec:
  containers:
  - image: "not a valid image!"
`)
	_, err := ScanImageReferences([]string{path})
	require.ErrorContains(t, err, `invalid image reference "not a valid image!"`)
}

// ----- RewriteImageReferences -----

func TestRewriteImageReferences(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "deploy.yaml", `kind: Pod
metadata:
  name: not-an-image
spec:
  containers:
  - name: main
    image: busybox:1.32
  - name: pinned
    image: quay.io/test/pinned@sha256:`+validSHA256+`
`)

	refToDigest := map[string]string{
		"docker.io/library/busybox:1.32": "sha256:" + validSHA256,
	}
	require.NoError(t, RewriteImageReferences([]string{path}, refToDigest))

	// The tag-form reference is pinned to the shipped digest; the already-digested reference
	// and the non-image scalar (metadata.name) are untouched.
	rewritten := decodeSingleDoc(t, path)
	containers := rewritten["spec"].(map[string]interface{})["containers"].([]interface{})
	assert.Equal(t, "docker.io/library/busybox@sha256:"+validSHA256, containers[0].(map[string]interface{})["image"])
	assert.Equal(t, "quay.io/test/pinned@sha256:"+validSHA256, containers[1].(map[string]interface{})["image"])
	assert.Equal(t, "not-an-image", rewritten["metadata"].(map[string]interface{})["name"])
}

func TestRewriteImageReferences_RejectsInvalidImageField(t *testing.T) {
	path := writeFile(t, t.TempDir(), "invalid.yaml", `kind: Pod
spec:
  containers:
  - image: "not a valid image!"
`)
	err := RewriteImageReferences([]string{path}, map[string]string{})
	require.ErrorContains(t, err, `invalid image reference "not a valid image!"`)
}

// TestRewriteImageReferences_UnpinnableFails doubles as the digest-presence check: a
// reference absent from the shipped set has no offline-pull target and fails the build
// (Codex review point 2).
func TestRewriteImageReferences_UnpinnableFails(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "deploy.yaml", `kind: Pod
spec:
  containers:
  - name: main
    image: quay.io/test/missing:1.0
`)

	// present digest passes...
	require.NoError(t, RewriteImageReferences([]string{path},
		map[string]string{"quay.io/test/missing:1.0": "sha256:" + validSHA256}))

	// ...absent digest fails the build.
	path2 := writeFile(t, dir, "deploy2.yaml", `kind: Pod
spec:
  containers:
  - name: main
    image: quay.io/test/missing:1.0
`)
	err := RewriteImageReferences([]string{path2}, map[string]string{})
	assert.ErrorContains(t, err, "cannot be pinned to a digest")
}

// TestRewriteImageReferences_Preservation characterizes semantic preservation (Codex review
// point 5): comments, anchors/aliases, and multi-document files survive meaning-equivalent,
// with only tag references pinned. It deliberately does not assert byte-for-byte formatting.
func TestRewriteImageReferences_Preservation(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "multi.yaml", `# top comment
kind: ConfigMap
data:
  name: &shared keep-me
  alias: *shared
spec:
  containers:
  - name: main
    image: busybox:1.32 # trailing comment
---
kind: Secret
data:
  value: untouched
`)

	require.NoError(t, RewriteImageReferences([]string{path},
		map[string]string{"docker.io/library/busybox:1.32": "sha256:" + validSHA256}))

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	// Both documents survive, meaning-equivalent.
	docs := decodeAllDocs(t, data)
	require.Len(t, docs, 2)
	// The anchor/alias still resolves to the shared value.
	cmData := docs[0]["data"].(map[string]interface{})
	assert.Equal(t, "keep-me", cmData["name"])
	assert.Equal(t, "keep-me", cmData["alias"])
	// The image scalar is pinned; the second document is untouched.
	containers := docs[0]["spec"].(map[string]interface{})["containers"].([]interface{})
	assert.Equal(t, "docker.io/library/busybox@sha256:"+validSHA256, containers[0].(map[string]interface{})["image"])
	assert.Equal(t, "untouched", docs[1]["data"].(map[string]interface{})["value"])
	// Comments are preserved by the yaml.v3 node round-trip (documented behavior).
	assert.Contains(t, string(data), "# top comment")
}

// TestRewriteImageReferences_Alias proves an aliased image reference (image: *img) whose
// anchor is defined under a non-image field is pinned, not left tag-based: the scanner
// resolves the alias, so the reference must be rewritten here too. The anchored scalar is
// pinned in place, so the anchor and the alias both resolve to the digest.
func TestRewriteImageReferences_Alias(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "alias.yaml", `kind: Pod
metadata:
  labels:
    shared: &img busybox:1.32
spec:
  containers:
  - name: main
    image: *img
`)

	require.NoError(t, RewriteImageReferences([]string{path},
		map[string]string{"docker.io/library/busybox:1.32": "sha256:" + validSHA256}))

	rewritten := decodeSingleDoc(t, path)
	pinned := "docker.io/library/busybox@sha256:" + validSHA256
	// Both the aliased image field and the anchored scalar resolve to the pinned digest.
	containers := rewritten["spec"].(map[string]interface{})["containers"].([]interface{})
	assert.Equal(t, pinned, containers[0].(map[string]interface{})["image"])
	assert.Equal(t, pinned, rewritten["metadata"].(map[string]interface{})["labels"].(map[string]interface{})["shared"])
}

// TestRewriteImageReferences_ImageSuffixField proves a short reference under a custom
// *Image-suffixed key is pinned (the companion to the scan-side coverage).
func TestRewriteImageReferences_ImageSuffixField(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "cr.yaml", `apiVersion: example.com/v1
kind: MyOperator
spec:
  operandImage: busybox:1.32
`)

	require.NoError(t, RewriteImageReferences([]string{path},
		map[string]string{"docker.io/library/busybox:1.32": "sha256:" + validSHA256}))

	rewritten := decodeSingleDoc(t, path)
	assert.Equal(t, "docker.io/library/busybox@sha256:"+validSHA256,
		rewritten["spec"].(map[string]interface{})["operandImage"])
}

// ----- ScanMirrorResources -----

func idmsManifest(source, mirror, policy string) string {
	return `apiVersion: config.openshift.io/v1
kind: ImageDigestMirrorSet
metadata:
  name: test
spec:
  imageDigestMirrors:
  - source: ` + source + `
    mirrors:
    - ` + mirror + `
    mirrorSourcePolicy: ` + policy + `
`
}

func itmsManifest(source, mirror, policy string) string {
	return `apiVersion: config.openshift.io/v1
kind: ImageTagMirrorSet
metadata:
  name: test
spec:
  imageTagMirrors:
  - source: ` + source + `
    mirrors:
    - ` + mirror + `
    mirrorSourcePolicy: ` + policy + `
`
}

func TestScanMirrorResources_Pass(t *testing.T) {
	dir := t.TempDir()
	// IDMS and ITMS with the local host (one with a repository path) and NeverContactSource.
	idms := writeFile(t, dir, "idms.yaml", idmsManifest("quay.io/openshift-release-dev/ocp-release", "localhost:5000", "NeverContactSource"))
	itms := writeFile(t, dir, "itms.yaml", itmsManifest("quay.io/openshift-release-dev/ocp-release", "localhost:5000/openshift/release", "NeverContactSource"))
	// A non-mirror manifest is ignored.
	other := writeFile(t, dir, "cm.yaml", "apiVersion: v1\nkind: ConfigMap\ndata:\n  a: b\n")

	assert.NoError(t, ScanMirrorResources([]string{idms, itms, other}))
}

func TestScanMirrorResources_Fail(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name     string
		content  string
		contains string
	}{
		{"idms external host", idmsManifest("quay.io/x/y", "mirror.example.com:5000", "NeverContactSource"), "mirrors to host"},
		{"idms allow policy", idmsManifest("quay.io/x/y", "localhost:5000", "AllowContactingSource"), "mirrorSourcePolicy"},
		{"idms missing policy", idmsManifest("quay.io/x/y", "localhost:5000", `""`), "mirrorSourcePolicy"},
		{"itms external host", itmsManifest("quay.io/x/y", "mirror.example.com:5000", "NeverContactSource"), "mirrors to host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, dir, tc.name+".yaml", tc.content)
			assert.ErrorContains(t, ScanMirrorResources([]string{path}), tc.contains)
		})
	}

	t.Run("any ICSP rejected", func(t *testing.T) {
		icsp := writeFile(t, dir, "icsp.yaml", `apiVersion: operator.openshift.io/v1alpha1
kind: ImageContentSourcePolicy
metadata:
  name: test
spec:
  repositoryDigestMirrors:
  - source: quay.io/x/y
    mirrors:
    - localhost:5000
`)
		assert.ErrorContains(t, ScanMirrorResources([]string{icsp}), "not permitted on self-contained media")
	})

	// A mirror resource smuggled inside a List must still be rejected: post-pivot unwraps
	// items[] and applies each member verbatim, so the scan must recurse into it.
	t.Run("list-wrapped ICSP rejected", func(t *testing.T) {
		list := writeFile(t, dir, "list-icsp.yaml", `apiVersion: v1
kind: List
items:
- apiVersion: operator.openshift.io/v1alpha1
  kind: ImageContentSourcePolicy
  metadata:
    name: test
  spec:
    repositoryDigestMirrors:
    - source: quay.io/x/y
      mirrors:
      - localhost:5000
`)
		assert.ErrorContains(t, ScanMirrorResources([]string{list}), "not permitted on self-contained media")
	})

	t.Run("list-wrapped external IDMS rejected", func(t *testing.T) {
		list := writeFile(t, dir, "list-idms.yaml", `apiVersion: v1
kind: List
items:
- `+indentManifest(idmsManifest("quay.io/x/y", "mirror.example.com:5000", "NeverContactSource"))+`
`)
		assert.ErrorContains(t, ScanMirrorResources([]string{list}), "mirrors to host")
	})
}

func TestScanMirrorResources_ListWrappedCompliantPasses(t *testing.T) {
	dir := t.TempDir()
	list := writeFile(t, dir, "list-ok.yaml", `apiVersion: v1
kind: List
items:
- `+indentManifest(idmsManifest("quay.io/x/y", "localhost:5000/x/y", "NeverContactSource"))+`
`)
	assert.NoError(t, ScanMirrorResources([]string{list}))
}

// indentManifest indents a manifest document by two spaces so it can be embedded as a
// single YAML list item (after a "- " marker).
func indentManifest(doc string) string {
	lines := strings.Split(strings.TrimRight(doc, "\n"), "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = "  " + lines[i]
	}
	return strings.Join(lines, "\n")
}

// ----- buildImageLayout wiring (self-contained end-to-end) -----

func TestBuildImageLayoutSelfContainedRendersAndPins(t *testing.T) {
	srcDir := t.TempDir()
	// A cluster manifest referencing a p3-only image by tag, plus a compliant local IDMS.
	clusterManifest := writeFile(t, srcDir, "workload.yaml", `kind: Pod
spec:
  containers:
  - name: main
    image: quay.io/p3/app:1.0
`)
	idms := writeFile(t, srcDir, "idms.yaml", idmsManifest("quay.io/p3/app", "localhost:5000/p3/app", "NeverContactSource"))

	m := validSelfContained()
	m.ClusterManifests = &[]ManifestSource{{Path: clusterManifest}, {Path: idms}}

	dir := t.TempDir()
	p2Root := filepath.Join(dir, "p2")
	p3Root := filepath.Join(dir, "p3")
	outputPath := filepath.Join(dir, "usb.img")

	deps := imageBuildDeps{
		seedReader: fakeSeedReader{list: []string{"quay.io/seed/a@sha256:" + validSHA256}},
		mirrorer:   fakeMirrorer{},
	}

	result, err := buildImageLayout(context.Background(), nil, m, ImageSetOptions{RecertImage: testRecertImage, LCAImage: testLCAImage}, outputPath, p2Root, p3Root, deps)
	require.NoError(t, err)
	require.NotNil(t, result)

	// The p3 image was scanned, mirrored, and unioned into the shipped set.
	var names []string
	for _, e := range result.Images {
		names = append(names, e.Name)
	}
	assert.Contains(t, names, "quay.io/p3/app")

	// The shipped manifest's tag reference is pinned to the digest actually mirrored.
	rewritten := decodeSingleDoc(t, filepath.Join(p3Root, common.ClusterConfigDir, common.ManifestsDir, "workload.yaml"))
	image := rewritten["spec"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})["image"].(string)
	assert.Equal(t, "quay.io/p3/app@"+fakeDigest("quay.io/p3/app:1.0"), image)

	// manifest.json was rendered.
	assert.FileExists(t, filepath.Join(p3Root, common.ClusterConfigDir, common.SeedReconfigurationFileName))
}

func TestBuildImageLayoutSelfContainedRejectsExternalMirror(t *testing.T) {
	srcDir := t.TempDir()
	idms := writeFile(t, srcDir, "idms.yaml", idmsManifest("quay.io/x/y", "mirror.example.com:5000", "NeverContactSource"))

	m := validSelfContained()
	m.ClusterManifests = &[]ManifestSource{{Path: idms}}

	dir := t.TempDir()
	deps := imageBuildDeps{
		seedReader: fakeSeedReader{err: assert.AnError}, // must not be reached: the scan fails first
		mirrorer:   fakeMirrorer{},
	}
	_, err := buildImageLayout(context.Background(), nil, m, ImageSetOptions{RecertImage: testRecertImage, LCAImage: testLCAImage},
		filepath.Join(dir, "usb.img"), filepath.Join(dir, "p2"), filepath.Join(dir, "p3"), deps)
	assert.ErrorContains(t, err, "validating p3 mirror resources")
	assert.NoDirExists(t, filepath.Join(dir, "p3"))
}

func TestBuildImageLayoutSelfContainedCleansP3AfterMirrorFailure(t *testing.T) {
	m := validSelfContained()
	dir := t.TempDir()
	m.ClusterManifests = &[]ManifestSource{{Path: writeFile(t, dir, "cluster.yaml", "kind: ConfigMap\n")}}
	p3Root := filepath.Join(dir, "p3")
	deps := imageBuildDeps{
		seedReader: fakeSeedReader{list: []string{"quay.io/seed/a@sha256:" + validSHA256}},
		mirrorer: fakeMirrorer{failOn: map[string]bool{
			"quay.io/seed/a@sha256:" + validSHA256: true,
		}},
	}
	_, err := buildImageLayout(context.Background(), nil, m, ImageSetOptions{RecertImage: testRecertImage, LCAImage: testLCAImage},
		filepath.Join(dir, "usb.img"), filepath.Join(dir, "p2"), p3Root, deps)
	assert.ErrorContains(t, err, "failed to mirror")
	assert.NoDirExists(t, p3Root)
}

func TestBuildImageLayoutInstallOnlySkipsP3(t *testing.T) {
	m := validInstallOnly()
	m.ExtraPrecacheImages = nil

	dir := t.TempDir()
	p3Root := filepath.Join(dir, "p3")
	deps := imageBuildDeps{
		seedReader: fakeSeedReader{list: []string{"quay.io/seed/a@sha256:" + validSHA256}},
		mirrorer:   fakeMirrorer{},
	}
	_, err := buildImageLayout(context.Background(), nil, m, ImageSetOptions{RecertImage: testRecertImage, LCAImage: testLCAImage},
		filepath.Join(dir, "usb.img"), filepath.Join(dir, "p2"), p3Root, deps)
	require.NoError(t, err)
	// install-only ships no p3 tree.
	assert.NoDirExists(t, p3Root)
}

// ----- decode helpers -----

func decodeSingleDoc(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	docs := decodeAllDocs(t, data)
	require.Len(t, docs, 1)
	return docs[0]
}

func decodeAllDocs(t *testing.T, data []byte) []map[string]interface{} {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var docs []map[string]interface{}
	for {
		var doc map[string]interface{}
		err := dec.Decode(&doc)
		if err != nil {
			break
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
	return docs
}
