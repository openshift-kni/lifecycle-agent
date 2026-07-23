package workload

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"go.uber.org/mock/gomock"
)

func withMockExecutor(t *testing.T, fn func(*ops.MockExecute)) {
	t.Helper()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mock := ops.NewMockExecute(ctrl)
	orig := Executor
	Executor = mock
	defer func() { Executor = orig }()

	fn(mock)
}

func TestImageSupportsArch_ManifestListMatchingArch(t *testing.T) {
	withMockExecutor(t, func(mock *ops.MockExecute) {
		manifestJSON := fmt.Sprintf(`{
			"mediaType": "application/vnd.docker.distribution.manifest.list.v2+json",
			"manifests": [
				{"platform": {"architecture": "%s", "os": "linux"}}
			]
		}`, runtime.GOARCH)

		mock.EXPECT().Execute("podman", "manifest", "inspect", "example.com/image:latest", "--authfile", "/auth.json").
			Return(manifestJSON, nil)

		if !imageSupportsArch("example.com/image:latest", "/auth.json") {
			t.Error("expected image to be supported for current arch")
		}
	})
}

func TestImageSupportsArch_ManifestListNoMatchingArch(t *testing.T) {
	withMockExecutor(t, func(mock *ops.MockExecute) {
		manifestJSON := `{
			"mediaType": "application/vnd.docker.distribution.manifest.list.v2+json",
			"manifests": [
				{"platform": {"architecture": "s390x", "os": "linux"}}
			]
		}`

		mock.EXPECT().Execute("podman", "manifest", "inspect", "example.com/image:latest", "--authfile", "/auth.json").
			Return(manifestJSON, nil)

		if imageSupportsArch("example.com/image:latest", "/auth.json") {
			t.Error("expected image to be unsupported for current arch")
		}
	})
}

func TestImageSupportsArch_OCIImageIndexMatchingArch(t *testing.T) {
	withMockExecutor(t, func(mock *ops.MockExecute) {
		manifestJSON := fmt.Sprintf(`{
			"mediaType": "application/vnd.oci.image.index.v1+json",
			"manifests": [
				{"platform": {"architecture": "s390x", "os": "linux"}},
				{"platform": {"architecture": "%s", "os": "linux"}}
			]
		}`, runtime.GOARCH)

		mock.EXPECT().Execute("podman", "manifest", "inspect", "example.com/image:latest", "--authfile", "/auth.json").
			Return(manifestJSON, nil)

		if !imageSupportsArch("example.com/image:latest", "/auth.json") {
			t.Error("expected image to be supported for current arch via OCI index")
		}
	})
}

func TestImageSupportsArch_InspectFails(t *testing.T) {
	withMockExecutor(t, func(mock *ops.MockExecute) {
		mock.EXPECT().Execute("podman", "manifest", "inspect", "example.com/image:latest", "--authfile", "/auth.json").
			Return("", fmt.Errorf("network error"))

		if !imageSupportsArch("example.com/image:latest", "/auth.json") {
			t.Error("expected fallthrough to true when inspect fails")
		}
	})
}

func TestImageSupportsArch_InvalidJSON(t *testing.T) {
	withMockExecutor(t, func(mock *ops.MockExecute) {
		mock.EXPECT().Execute("podman", "manifest", "inspect", "example.com/image:latest", "--authfile", "/auth.json").
			Return("not json", nil)

		if !imageSupportsArch("example.com/image:latest", "/auth.json") {
			t.Error("expected fallthrough to true when JSON is invalid")
		}
	})
}

func TestImageSupportsArch_SingleManifest(t *testing.T) {
	withMockExecutor(t, func(mock *ops.MockExecute) {
		manifestJSON := `{
			"mediaType": "application/vnd.docker.distribution.manifest.v2+json",
			"config": {},
			"layers": []
		}`

		mock.EXPECT().Execute("podman", "manifest", "inspect", "example.com/image:latest", "--authfile", "/auth.json").
			Return(manifestJSON, nil)

		if !imageSupportsArch("example.com/image:latest", "/auth.json") {
			t.Error("expected single manifest to be treated as compatible")
		}
	})
}

func TestImageSupportsArch_NoAuthFile(t *testing.T) {
	withMockExecutor(t, func(mock *ops.MockExecute) {
		manifestJSON := fmt.Sprintf(`{
			"mediaType": "application/vnd.docker.distribution.manifest.list.v2+json",
			"manifests": [
				{"platform": {"architecture": "%s", "os": "linux"}}
			]
		}`, runtime.GOARCH)

		mock.EXPECT().Execute("podman", "manifest", "inspect", "example.com/image:latest").
			Return(manifestJSON, nil)

		if !imageSupportsArch("example.com/image:latest", "") {
			t.Error("expected image to be supported when no authfile is provided")
		}
	})
}

func TestImageSupportsArch_NilPlatform(t *testing.T) {
	withMockExecutor(t, func(mock *ops.MockExecute) {
		manifestJSON := `{
			"mediaType": "application/vnd.docker.distribution.manifest.list.v2+json",
			"manifests": [
				{"digest": "sha256:abc123"}
			]
		}`

		mock.EXPECT().Execute("podman", "manifest", "inspect", "example.com/image:latest", "--authfile", "/auth.json").
			Return(manifestJSON, nil)

		if imageSupportsArch("example.com/image:latest", "/auth.json") {
			t.Error("expected image with nil platform entries to be unsupported")
		}
	})
}
