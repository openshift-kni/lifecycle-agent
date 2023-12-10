package utils

import (
	"github.com/stretchr/testify/assert"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsIpv6(t *testing.T) {
	testcases := []struct {
		name     string
		ip       string
		expected bool
	}{
		{
			name:     "ipv6 - true",
			ip:       "2620:52:0:198::10",
			expected: true,
		},
		{
			name:     "ipv6 - false",
			ip:       "192,168.127.10",
			expected: false,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, IsIpv6(tc.ip), tc.expected)
		})
	}
}

func TestCopyFileIfExists(t *testing.T) {
	testcases := []struct {
		name          string
		expectedError bool
		fileExists    bool
	}{
		{
			name:          "Dest folder doesn't exist",
			expectedError: false,
			fileExists:    true,
		},
		{
			name:          "file exists",
			expectedError: false,
			fileExists:    true,
		},
		{
			name:          "file doesn't exist",
			expectedError: false,
			fileExists:    false,
		},
	}

	for _, tc := range testcases {
		tmpDir := t.TempDir()
		t.Run(tc.name, func(t *testing.T) {
			dst := filepath.Join(tmpDir, "destFolder")
			if !tc.expectedError {
				if err := os.MkdirAll(filepath.Join(tmpDir, "destFolder"), 0o700); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
			source := filepath.Join(tmpDir, "test")
			if tc.fileExists {
				f, err := os.Create(source)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				_ = f.Close()
			}

			err := CopyFileIfExists(source, filepath.Join(dst, "test"))
			assert.Equal(t, err != nil, tc.expectedError)
		})
	}
}

func TestCopyReplaceMirrorRegistry(t *testing.T) {
	image := "quay.io/openshift-kni/lifecycle-agent-operator:4.14.0 "
	testcases := []struct {
		name            string
		seedRegistry    string
		clusterRegistry string
		shouldChange    bool
	}{
		{
			name:            "shouldn't change",
			seedRegistry:    "aaa.io",
			clusterRegistry: "bbb.io",
			shouldChange:    false,
		},
		{
			name:            "should change",
			seedRegistry:    "quay.io",
			clusterRegistry: "bbb.io",
			shouldChange:    true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			newImage, err := ReplaceImageRegistry(image, tc.clusterRegistry, tc.seedRegistry)
			assert.Equal(t, err, nil)
			assert.Equal(t, strings.HasPrefix(newImage, tc.clusterRegistry), tc.shouldChange)
		})
	}
}
