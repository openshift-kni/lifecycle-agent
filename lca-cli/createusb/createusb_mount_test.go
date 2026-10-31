package createusb

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

func TestWithMountedImageLeavesPulledImageAvailable(t *testing.T) {
	mock := ops.NewMockOps(gomock.NewController(t))
	image := "quay.io/example/lca:latest"
	mock.EXPECT().RunInHostNamespace("podman", "pull", image).Return("", nil)
	mock.EXPECT().MountImage(image).Return("/image", nil)
	mock.EXPECT().RunInHostNamespace("podman", "image", "umount", image).Return("", nil)

	err := withMountedImage(mock, image, "", func(mountpoint string) error {
		require.Equal(t, "/image", mountpoint)
		return nil
	})
	require.NoError(t, err)
}

func TestWithMountedImageUnmountsAfterReadFailure(t *testing.T) {
	mock := ops.NewMockOps(gomock.NewController(t))
	image := "quay.io/example/lca:latest"
	mock.EXPECT().RunInHostNamespace("podman", "pull", image).Return("", nil)
	mock.EXPECT().MountImage(image).Return("/image", nil)
	mock.EXPECT().RunInHostNamespace("podman", "image", "umount", image).Return("", nil)

	readErr := errors.New("read failed")
	require.ErrorIs(t, withMountedImage(mock, image, "", func(string) error { return readErr }), readErr)
}

func TestWithMountedImageReturnsUnmountErrorAfterSuccess(t *testing.T) {
	mock := ops.NewMockOps(gomock.NewController(t))
	image := "quay.io/example/lca:latest"
	umountErr := errors.New("unmount failed")
	mock.EXPECT().RunInHostNamespace("podman", "pull", image).Return("", nil)
	mock.EXPECT().MountImage(image).Return("/image", nil)
	mock.EXPECT().RunInHostNamespace("podman", "image", "umount", image).Return("", umountErr)

	require.ErrorIs(t, withMountedImage(mock, image, "", func(string) error { return nil }), umountErr)
}

func TestWithMountedImagePreservesReadErrorWhenUnmountFails(t *testing.T) {
	mock := ops.NewMockOps(gomock.NewController(t))
	image := "quay.io/example/lca:latest"
	umountErr := errors.New("unmount failed")
	mock.EXPECT().RunInHostNamespace("podman", "pull", image).Return("", nil)
	mock.EXPECT().MountImage(image).Return("/image", nil)
	mock.EXPECT().RunInHostNamespace("podman", "image", "umount", image).Return("", umountErr)

	readErr := errors.New("read failed")
	err := withMountedImage(mock, image, "", func(string) error { return readErr })
	require.ErrorIs(t, err, readErr)
	require.NotErrorIs(t, err, umountErr)
}
