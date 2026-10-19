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

// This file owns rendering and validating the self-contained p3 configuration tree
// (USB-based IBI proposal §6.4 step 4 and §6.3 p3 rules). Partition creation and
// population live in createusb_p3.go. These are self-contained, unit-tested building
// blocks wired into the image pipeline by createusb_images.go:
//
//   - RenderP3Tree renders the /opt/openshift config tree (manifest.json +
//     cluster-configuration/manifests + extra-manifests) from the manifest's siteConfig
//     and manifest sources.
//   - ScanImageReferences discovers the image references in the rendered manifests
//     (known image fields first, a parser-validated fallback for custom resources) and
//     normalizes them to fully-qualified name:tag form.
//   - RewriteImageReferences pins every tag-form reference in the rendered manifests to
//     name@sha256:... using the digests actually mirrored for this build.
//   - ScanMirrorResources rejects any ImageDigestMirrorSet/ImageTagMirrorSet/
//     ImageContentSourcePolicy that could reach the network (proposal §6.3, CWE-16).
//
// install-only ships no p3 tree, so none of these run for that profile.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.podman.io/image/v5/docker/reference"
	configv1 "github.com/openshift/api/config/v1"
	yaml "gopkg.in/yaml.v3"

	"github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

const (
	// localRegistryEndpoint is the only mirror host self-contained media may reference.
	// Mirror resources whose mirror host is anything else can reach the network and are
	// rejected (proposal §6.3: the registry fallback allows only localhost:5000).
	localRegistryEndpoint = "localhost:5000"

	// imageFieldSuffix identifies a mapping key whose scalar value is an image reference.
	// A key matches when it is exactly "image" (containers[].image, initContainers[].image,
	// ephemeralContainers[].image, CatalogSource.spec.image) or, case-insensitively, ends in
	// "image" (operandImage, operatorImage, relatedImages' image, kubeRBACProxyImage, ...).
	// A scalar under such a key is accepted even when it is a bare name (busybox) or a short,
	// domain-less tag (busybox:1.32); elsewhere a stricter rule applies (see parseImageRef).
	imageFieldSuffix = "image"
)

const (
	configAPIVersion = "config.openshift.io/v1"
	idmsKind         = "ImageDigestMirrorSet"
	itmsKind         = "ImageTagMirrorSet"

	operatorAlphaAPIVersion = "operator.openshift.io/v1alpha1"
	icspKind                = "ImageContentSourcePolicy"
)

// P3RenderResult is returned by RenderP3Tree. Root is the rendered tree root; ManifestPaths
// is the ordered list of rendered manifest files (under cluster-configuration/manifests and
// extra-manifests) that the scan, rewrite, and mirror-resource passes operate on.
type P3RenderResult struct {
	Root          string
	ManifestPaths []string
}

// RenderP3Tree renders the self-contained /opt/openshift config tree under p3Root
// (proposal §6.4 step 4):
//
//   - cluster-configuration/manifest.json -- the SeedReconfiguration built from siteConfig
//     (api_version=1 so post-pivot's version check accepts it).
//   - cluster-configuration/manifests/   -- the clusterManifests sources.
//   - extra-manifests/                   -- the extraManifests sources.
//
// A directory source expands non-recursively in lexical order; sub-directories are skipped
// and every file is copied verbatim. A destination-name collision within a destination
// directory fails the build. network-configuration/ is intentionally left unpopulated: the
// current siteConfig schema carries only rawNMStateConfig (applied by post-pivot straight
// from manifest.json), so there is no source for system-connections files (see
// docs/create-usb.md).
func RenderP3Tree(m *Manifest, p3Root string) (*P3RenderResult, error) {
	if m.Mode != ModeSelfContained {
		return nil, fmt.Errorf("p3 config tree is only rendered for mode %q", ModeSelfContained)
	}
	if m.SiteConfig == nil {
		return nil, fmt.Errorf("siteConfig is required to render the p3 config tree")
	}

	// Render only into a fresh (absent or empty) destination. RenderP3Tree writes just the
	// current inputs and the later scan/rewrite/mirror passes operate only on the paths it
	// returns, so a manifest left over in p3Root from a previous build would never be scanned,
	// digest-pinned, or mirror-checked -- yet post-pivot would still apply it from the media.
	// Rejecting a nonempty destination keeps the shipped tree exactly the set this build
	// renders.
	if entries, err := os.ReadDir(p3Root); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("checking p3 config tree destination %q: %w", p3Root, err)
		}
	} else if len(entries) > 0 {
		return nil, fmt.Errorf("p3 config tree destination %q is not empty; render into a fresh directory so a stale manifest from a previous build cannot ship unscanned", p3Root)
	}

	clusterConfigDir := filepath.Join(p3Root, common.ClusterConfigDir)
	if err := os.MkdirAll(clusterConfigDir, 0o750); err != nil {
		return nil, fmt.Errorf("creating cluster-configuration directory: %w", err)
	}

	manifestJSON := filepath.Join(clusterConfigDir, common.SeedReconfigurationFileName)
	if err := utils.MarshalToFile(seedReconfigurationFromSiteConfig(m.SiteConfig), manifestJSON); err != nil {
		return nil, fmt.Errorf("writing %s: %w", manifestJSON, err)
	}

	var manifestPaths []string
	if m.ClusterManifests != nil {
		paths, err := renderManifestSources(*m.ClusterManifests, filepath.Join(clusterConfigDir, common.ManifestsDir))
		if err != nil {
			return nil, err
		}
		manifestPaths = append(manifestPaths, paths...)
	}
	if m.ExtraManifests != nil {
		paths, err := renderManifestSources(*m.ExtraManifests, filepath.Join(p3Root, common.ExtraManifestsDir))
		if err != nil {
			return nil, err
		}
		manifestPaths = append(manifestPaths, paths...)
	}

	return &P3RenderResult{Root: p3Root, ManifestPaths: manifestPaths}, nil
}

// seedReconfigurationFromSiteConfig maps the create-usb siteConfig to the on-media
// SeedReconfiguration post-pivot consumes. APIVersion is set to the only version
// post-pivot accepts; pullSecret is already resolved (ResolvePullSecret runs in Validate).
func seedReconfigurationFromSiteConfig(sc *SiteConfig) *seedreconfig.SeedReconfiguration {
	return &seedreconfig.SeedReconfiguration{
		APIVersion:       seedreconfig.SeedReconfigurationVersion,
		ClusterName:      sc.ClusterName,
		BaseDomain:       sc.BaseDomain,
		Hostname:         sc.Hostname,
		NodeIPs:          sc.NodeIPs,
		MachineNetworks:  sc.MachineNetworks,
		RawNMStateConfig: sc.RawNMStateConfig,
		SSHKey:           sc.SSHKey,
		PullSecret:       sc.PullSecret,
	}
}

// renderManifestSources copies every source into destDir and returns the ordered list of
// written files. Directory sources expand non-recursively in lexical order; a
// destination-name collision within destDir fails the build.
func renderManifestSources(sources []ManifestSource, destDir string) ([]string, error) {
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return nil, fmt.Errorf("creating manifest directory %q: %w", destDir, err)
	}
	seen := make(map[string]string)
	var written []string
	for _, src := range sources {
		files, err := expandManifestSource(src)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			base := filepath.Base(f)
			if prev, dup := seen[base]; dup {
				return nil, fmt.Errorf("destination-name collision in %q: %q and %q both render to %q", destDir, prev, f, base)
			}
			seen[base] = f
			dst := filepath.Join(destDir, base)
			if err := copyFileVerbatim(f, dst); err != nil {
				return nil, err
			}
			written = append(written, dst)
		}
	}
	return written, nil
}

// expandManifestSource returns the ordered files a manifest source contributes: a file
// source yields itself; a directory source yields its files in lexical order, skipping
// sub-directories (non-recursive, deterministic).
func expandManifestSource(src ManifestSource) ([]string, error) {
	info, err := os.Stat(src.Path)
	if err != nil {
		return nil, fmt.Errorf("accessing manifest source %q: %w", src.Path, err)
	}
	if !info.IsDir() {
		return []string{src.Path}, nil
	}
	// os.ReadDir returns entries sorted by filename, giving a deterministic lexical order.
	entries, err := os.ReadDir(src.Path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest directory %q: %w", src.Path, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue // non-recursive: sub-directories are not expanded
		}
		files = append(files, filepath.Join(src.Path, e.Name()))
	}
	return files, nil
}

// copyFileVerbatim copies src to dst byte-for-byte.
func copyFileVerbatim(src, dst string) error {
	data, err := os.ReadFile(filepath.Clean(src)) //nolint:gosec // src is a build-time manifest source referenced by the operator's manifest
	if err != nil {
		return fmt.Errorf("reading manifest source %q: %w", src, err)
	}
	//nolint:gosec // dst is a tool-controlled destDir joined with filepath.Base(src), so it cannot traverse out of the build-time p3 root
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return fmt.Errorf("writing rendered manifest %q: %w", dst, err)
	}
	return nil
}

// ScanImageReferences returns the normalized image references found in the rendered
// manifests (proposal §6.4 step 4). References are discovered known-image-fields-first,
// with a parser-validated fallback for custom resources, then normalized to fully-qualified
// name:tag form (a missing tag defaults to latest, matching Kubernetes). Already-digested
// references pass through unchanged. Decode errors fail closed.
func ScanImageReferences(manifestPaths []string) ([]string, error) {
	var images []string
	for _, path := range manifestPaths {
		docs, err := decodeManifestDocs(path)
		if err != nil {
			return nil, err
		}
		for _, doc := range docs {
			if err := collectImages(doc, false, &images); err != nil {
				return nil, fmt.Errorf("scanning image references in %q: %w", path, err)
			}
		}
	}
	return images, nil
}

// isImageFieldKey reports whether a mapping key names an image-reference field: the exact
// key "image" or any key that ends in "image" case-insensitively (operandImage,
// operatorImage, relatedImages' image, ...). A scalar under such a key is treated as an
// image reference even when it is a short, domain-less form (busybox:1.32), so a
// custom-resource image field is discovered and pinned instead of being silently skipped by
// the stricter generic fallback. A non-image value under such a key has no shipped digest
// and fails the build loudly, which is the intended explicit-failure behavior.
func isImageFieldKey(key string) bool {
	return strings.HasSuffix(strings.ToLower(key), imageFieldSuffix)
}

// collectImages recursively collects normalized image references from a decoded manifest
// value. underImageKey records whether the value sits directly under an image field (see
// isImageFieldKey): such a value is accepted even as a bare or short name, while elsewhere
// parseImageRef applies the stricter custom-resource rule (see parseImageRef). Map keys are
// visited in sorted order so the result is deterministic.
func collectImages(node interface{}, underImageKey bool, out *[]string) error {
	switch v := node.(type) {
	case map[string]interface{}:
		if underImageKey {
			return fmt.Errorf("image field must contain a valid image reference, got a mapping")
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := collectImages(v[k], isImageFieldKey(k), out); err != nil {
				return fmt.Errorf("field %q: %w", k, err)
			}
		}
	case []interface{}:
		if underImageKey {
			return fmt.Errorf("image field must contain a valid image reference, got a sequence")
		}
		for _, item := range v {
			if err := collectImages(item, false, out); err != nil {
				return err
			}
		}
	case string:
		if ref, ok := parseImageRef(v, underImageKey); ok {
			*out = utils.AppendToListIfNotExists(*out, reference.TagNameOnly(ref).String())
		} else if underImageKey {
			return fmt.Errorf("invalid image reference %q", v)
		}
	default:
		if underImageKey {
			return fmt.Errorf("image field must contain a valid image reference, got %T", node)
		}
	}
	return nil
}

// parseImageRef parses s as an image reference. underImageKey selects the acceptance rule
// (Codex review point 1):
//
//   - true (the value of a known image field, see isImageFieldKey): any string that parses
//     as a reference is accepted, including a bare name (busybox) or a short tag
//     (busybox:1.32); it is normalized to fully-qualified name:tag by the caller.
//   - false (the generic custom-resource walk): accepted only when the string clearly looks
//     like an image -- an explicit registry domain in the first path component AND an
//     explicit tag or digest -- so apiVersion values (apps/v1, networking.k8s.io/v1), plain
//     words, and URLs are not mistaken for images.
//
// It returns the parsed, normalized reference and whether s is an acceptable image reference.
func parseImageRef(s string, underImageKey bool) (reference.Named, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	ref, err := reference.ParseNormalizedNamed(s)
	if err != nil {
		return nil, false
	}
	if underImageKey {
		return ref, true
	}
	// Generic fallback: require both a registry domain and an explicit tag/digest to avoid
	// collecting non-image values that happen to parse as docker.io/library references.
	_, tagged := ref.(reference.Tagged)
	_, digested := ref.(reference.Digested)
	if (!tagged && !digested) || !hasRegistryDomain(s) {
		return nil, false
	}
	return ref, true
}

// hasRegistryDomain reports whether s has an explicit registry host: a "/" with a first
// component that contains a "." or ":" (e.g. registry.example.com/... or localhost:5000/...).
// A bare "apps/v1" (no dot/colon before the slash) and a tagless name do not qualify.
func hasRegistryDomain(s string) bool {
	i := strings.Index(s, "/")
	if i < 0 {
		return false
	}
	return strings.ContainsAny(s[:i], ".:")
}

// RewriteImageReferences pins every tag-form image reference in the rendered manifests to
// name@sha256:... using refToDigest, whose keys are the normalized references produced by
// ScanImageReferences and whose values are the digests mirrored for this build (proposal
// §5.2). A tag-form reference absent from refToDigest cannot be served offline and fails
// the build; already-digested references are left unchanged.
//
// The rewrite edits yaml.v3 scalar nodes in place, so the guarantee is semantic
// preservation -- the rewritten manifest is meaning-equivalent to the source except that
// tag references are pinned -- NOT byte-for-byte formatting (a node round-trip may alter
// quoting/indentation outside the edited scalar; see docs/create-usb.md).
func RewriteImageReferences(manifestPaths []string, refToDigest map[string]string) error {
	for _, path := range manifestPaths {
		if err := rewriteManifestFile(path, refToDigest); err != nil {
			return err
		}
	}
	return nil
}

func rewriteManifestFile(path string, refToDigest map[string]string) error {
	data, err := os.ReadFile(filepath.Clean(path)) //nolint:gosec // path is a rendered manifest the tool wrote under the build-time p3 root
	if err != nil {
		return fmt.Errorf("reading manifest %q: %w", path, err)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		if err := decoder.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("decoding manifest %q: %w", path, err)
		}
		docs = append(docs, &doc)
	}

	changed := false
	for _, doc := range docs {
		c, err := rewriteNode(doc, false, refToDigest, path)
		if err != nil {
			return err
		}
		changed = changed || c
	}
	if !changed {
		return nil
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, doc := range docs {
		if err := enc.Encode(doc); err != nil {
			_ = enc.Close()
			return fmt.Errorf("encoding rewritten manifest %q: %w", path, err)
		}
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("finalizing rewritten manifest %q: %w", path, err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("writing rewritten manifest %q: %w", path, err)
	}
	return nil
}

// rewriteNode walks a yaml.v3 node tree, pinning tag-form image scalars. underImageKey
// mirrors collectImages: a scalar directly under an image mapping key (see isImageFieldKey)
// is treated as an image even when bare.
func rewriteNode(node *yaml.Node, underImageKey bool, refToDigest map[string]string, path string) (bool, error) {
	changed := false
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range node.Content {
			ch, err := rewriteNode(c, false, refToDigest, path)
			if err != nil {
				return false, err
			}
			changed = changed || ch
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			isImg := key.Kind == yaml.ScalarNode && isImageFieldKey(key.Value)
			ch, err := rewriteNode(node.Content[i+1], isImg, refToDigest, path)
			if err != nil {
				return false, err
			}
			changed = changed || ch
		}
	case yaml.AliasNode:
		// An aliased value (image: *img) resolves to its anchored node. The generic decode the
		// scanner uses resolves aliases, so the reference is in refToDigest; follow the alias
		// here with the same key context and pin the anchored scalar in place so it is not left
		// tag-based. The anchor and every alias to it then render the shared, pinned value.
		if node.Alias != nil {
			return rewriteNode(node.Alias, underImageKey, refToDigest, path)
		}
	case yaml.ScalarNode:
		return rewriteScalar(node, underImageKey, refToDigest, path)
	}
	return changed, nil
}

func rewriteScalar(node *yaml.Node, underImageKey bool, refToDigest map[string]string, path string) (bool, error) {
	ref, ok := parseImageRef(node.Value, underImageKey)
	if !ok {
		if underImageKey {
			return false, fmt.Errorf("invalid image reference %q in %q", node.Value, path)
		}
		return false, nil
	}
	if _, digested := ref.(reference.Digested); digested {
		return false, nil // already pinned to a digest
	}
	norm := reference.TagNameOnly(ref).String()
	dgst, ok := refToDigest[norm]
	if !ok {
		return false, fmt.Errorf("image reference %q in manifest %q cannot be pinned to a digest (not present in the shipped image set)", node.Value, path)
	}
	node.Value = ref.Name() + "@" + dgst
	return true, nil
}

// ScanMirrorResources fails the build on any mirror resource that could reach the network
// (proposal §6.3, CWE-16). It covers the three resource types that configure image
// mirrors:
//
//   - ImageDigestMirrorSet / ImageTagMirrorSet (config.openshift.io/v1): every mirror host
//     must be the on-node local registry (localhost:5000, repository path allowed) and the
//     entry's mirrorSourcePolicy must be NeverContactSource.
//   - ImageContentSourcePolicy (operator.openshift.io/v1alpha1): rejected outright -- ICSP
//     has no mirrorSourcePolicy and always falls back to contacting the source, so it can
//     never satisfy the offline rule (documented strict policy in docs/create-usb.md).
func ScanMirrorResources(manifestPaths []string) error {
	for _, path := range manifestPaths {
		docs, err := decodeManifestDocs(path)
		if err != nil {
			return err
		}
		for _, doc := range docs {
			if err := scanDocForMirrorResources(doc, path); err != nil {
				return err
			}
		}
	}
	return nil
}

// scanDocForMirrorResources validates one manifest document, unwrapping List-style
// containers. post-pivot's handleManifest (postpivot.go) applies every member of a
// document's items[] verbatim, so a mirror resource smuggled inside a List must be
// rejected too -- otherwise it would bypass the scan here yet still be applied on the node.
func scanDocForMirrorResources(doc interface{}, path string) error {
	m, ok := doc.(map[string]interface{})
	if !ok {
		return nil
	}
	if items, ok := m["items"].([]interface{}); ok {
		for _, item := range items {
			if err := scanDocForMirrorResources(item, path); err != nil {
				return err
			}
		}
		return nil
	}
	apiVersion, _ := m["apiVersion"].(string)
	kind, _ := m["kind"].(string)
	return validateMirrorResourceDoc(apiVersion, kind, doc, path)
}

func validateMirrorResourceDoc(apiVersion, kind string, doc interface{}, path string) error {
	switch {
	case apiVersion == configAPIVersion && kind == idmsKind:
		var idms configv1.ImageDigestMirrorSet
		if err := decodeTyped(doc, &idms); err != nil {
			return fmt.Errorf("decoding %s in %q: %w", idmsKind, path, err)
		}
		for _, e := range idms.Spec.ImageDigestMirrors {
			if err := validateMirrorEntry(idmsKind, e.Source, e.Mirrors, e.MirrorSourcePolicy, path); err != nil {
				return err
			}
		}
	case apiVersion == configAPIVersion && kind == itmsKind:
		var itms configv1.ImageTagMirrorSet
		if err := decodeTyped(doc, &itms); err != nil {
			return fmt.Errorf("decoding %s in %q: %w", itmsKind, path, err)
		}
		for _, e := range itms.Spec.ImageTagMirrors {
			if err := validateMirrorEntry(itmsKind, e.Source, e.Mirrors, e.MirrorSourcePolicy, path); err != nil {
				return err
			}
		}
	case apiVersion == operatorAlphaAPIVersion && kind == icspKind:
		return fmt.Errorf("%s in %q is not permitted on self-contained media: it has no mirrorSourcePolicy and always falls back to contacting the source", icspKind, path)
	}
	return nil
}

// validateMirrorEntry enforces the local-only + NeverContactSource rule on one IDMS/ITMS
// mirror entry.
func validateMirrorEntry(kind, source string, mirrors []configv1.ImageMirror, policy configv1.MirrorSourcePolicy, path string) error {
	if policy != configv1.NeverContactSource {
		return fmt.Errorf("%s for source %q in %q sets mirrorSourcePolicy %q; self-contained media requires %q so the node never reaches the network",
			kind, source, path, policyOrUnset(policy), configv1.NeverContactSource)
	}
	for _, mirror := range mirrors {
		if host := mirrorHost(string(mirror)); host != localRegistryEndpoint {
			return fmt.Errorf("%s for source %q in %q mirrors to host %q (from %q); self-contained media only allows the on-node local registry %q",
				kind, source, path, host, string(mirror), localRegistryEndpoint)
		}
	}
	return nil
}

// mirrorHost returns the host[:port] portion of a mirror value (host[:port]/repository/path),
// so a valid local mirror with a repository path (localhost:5000/openshift/release) is
// matched on its host (Codex review point 2).
func mirrorHost(mirror string) string {
	if i := strings.Index(mirror, "/"); i >= 0 {
		return mirror[:i]
	}
	return mirror
}

func policyOrUnset(policy configv1.MirrorSourcePolicy) string {
	if policy == "" {
		return "<unset>"
	}
	return string(policy)
}

// decodeManifestDocs decodes every document in a (possibly multi-document) YAML or JSON
// manifest file into generic values, the same way post-pivot reads manifests.
func decodeManifestDocs(path string) ([]interface{}, error) {
	decoder, err := utils.GetYamlOrJsonDecoder(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest %q: %w", path, err)
	}
	var docs []interface{}
	for {
		var doc interface{}
		if err := decoder.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decoding manifest %q: %w", path, err)
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
	return docs, nil
}

// decodeTyped re-decodes a generic manifest document into a typed resource via its JSON
// tags.
func decodeTyped(doc interface{}, out interface{}) error {
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("re-marshaling manifest document: %w", err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decoding typed manifest resource: %w", err)
	}
	return nil
}
