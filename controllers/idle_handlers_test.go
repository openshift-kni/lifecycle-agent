/*
Copyright 2023.

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

package controllers

import (
	"fmt"
	"os"
	"testing"

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/ibu-imager/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"go.uber.org/mock/gomock"
)

func TestImageBasedUpgradeReconciler_cleanupUnbootedStateroot(t *testing.T) {
	tests := []struct {
		name            string
		wantErr         bool
		deployments     []rpmostreeclient.Deployment
		undeployIndices []int
		input           string
		expectToRemove  string
	}{
		{
			name:           "no deployments",
			wantErr:        false,
			deployments:    []rpmostreeclient.Deployment{},
			input:          "rhcos",
			expectToRemove: "rhcos",
		},
		{
			name:        "one deployment",
			wantErr:     true,
			deployments: []rpmostreeclient.Deployment{{OSName: "rhcos_4.10.15", Booted: true}},
			input:       "rhcos_4.10.15",
		},
		{
			name:    "two deployments, remove second",
			wantErr: false,
			deployments: []rpmostreeclient.Deployment{
				{OSName: "rhcos_4.10.15", Booted: true},
				{OSName: "rhcos", Booted: false},
			},
			undeployIndices: []int{1},
			input:           "rhcos",
			expectToRemove:  "rhcos",
		},
		{
			name:    "two deployments, remove first",
			wantErr: false,
			deployments: []rpmostreeclient.Deployment{
				{OSName: "rhcos_4.10.15", Booted: false},
				{OSName: "rhcos", Booted: true},
			},
			undeployIndices: []int{0},
			input:           "rhcos_4.10.15",
			expectToRemove:  "rhcos_4.10.15",
		},
		{
			name:    "three deployments",
			wantErr: false,
			deployments: []rpmostreeclient.Deployment{
				{OSName: "rhcos_4.10.11", Booted: false},
				{OSName: "rhcos_4.10.15", Booted: true},
				{OSName: "rhcos", Booted: false},
			},
			undeployIndices: []int{0},
			input:           "rhcos_4.10.11",
			expectToRemove:  "rhcos_4.10.11",
		},
		{
			// two deployments in one stateroot, one of them is booted.
			// we should not remove stateroot
			name:    "don't remove booted stateroot",
			wantErr: true,
			deployments: []rpmostreeclient.Deployment{
				{OSName: "rhcos_4.10.11", Booted: false},
				{OSName: "rhcos", Booted: true},
				{OSName: "rhcos", Booted: false},
			},
			undeployIndices: []int{},
			input:           "rhcos",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			ostreeclientMock := ostreeclient.NewMockIClient(ctrl)
			rpmostreeclientMock := rpmostreeclient.NewMockIClient(ctrl)
			executorMock := ops.NewMockExecute(ctrl)

			rpmostreeclientMock.EXPECT().QueryStatus().Return(&rpmostreeclient.Status{
				Deployments: tt.deployments}, nil)
			for _, x := range tt.undeployIndices {
				ostreeclientMock.EXPECT().Undeploy(x)
			}
			if tt.expectToRemove != "" {
				executorMock.EXPECT().Execute("unshare", "-m", "/bin/sh", "-c",
					fmt.Sprintf("\"mount -o remount,rw /sysroot && rm -rf /ostree/deploy/%s\"",
						tt.expectToRemove))
			}
			r := &ImageBasedUpgradeReconciler{
				Log:             logr.Discard(),
				RPMOstreeClient: rpmostreeclientMock,
				Executor:        executorMock,
				OstreeClient:    ostreeclientMock,
			}
			osStat = func(name string) (os.FileInfo, error) {
				return os.Stat(".")
			}

			if err := r.cleanupUnbootedStateroot(tt.input); (err != nil) != tt.wantErr {
				t.Errorf("ImageBasedUpgradeReconciler.cleanupUnbootedStateroot() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestImageBasedUpgradeReconciler_cleanupUnbootedStateroots(t *testing.T) {
	tests := []struct {
		name               string
		wantErr            bool
		deployments        []rpmostreeclient.Deployment
		undeployIndices    []int
		staterootsToRemove []string
	}{
		{
			name:        "no deployments",
			wantErr:     false,
			deployments: []rpmostreeclient.Deployment{},
		},
		{
			name:        "one deployment",
			wantErr:     false,
			deployments: []rpmostreeclient.Deployment{{OSName: "rhcos_4.10.15", Booted: true}},
		},
		{
			name:    "two deployments, remove second",
			wantErr: false,
			deployments: []rpmostreeclient.Deployment{
				{OSName: "rhcos_4.10.15", Booted: true},
				{OSName: "rhcos", Booted: false},
			},
			undeployIndices:    []int{1},
			staterootsToRemove: []string{"rhcos"},
		},
		{
			name:    "two deployments, remove first",
			wantErr: false,
			deployments: []rpmostreeclient.Deployment{
				{OSName: "rhcos_4.10.15", Booted: false},
				{OSName: "rhcos", Booted: true},
			},
			undeployIndices:    []int{0},
			staterootsToRemove: []string{"rhcos_4.10.15"},
		},
		{
			name:    "three deployments",
			wantErr: false,
			deployments: []rpmostreeclient.Deployment{
				{OSName: "rhcos_4.10.11", Booted: false},
				{OSName: "rhcos_4.10.15", Booted: true},
				{OSName: "rhcos", Booted: false},
			},
			undeployIndices:    []int{2, 0},
			staterootsToRemove: []string{"rhcos", "rhcos_4.10.11"},
		},
		{
			// two deployments in one stateroot, one of them is booted.
			// we should not remove stateroot
			name:    "don't remove booted stateroot",
			wantErr: false,
			deployments: []rpmostreeclient.Deployment{
				{OSName: "rhcos_4.10.11", Booted: false},
				{OSName: "rhcos", Booted: true},
				{OSName: "rhcos", Booted: false},
			},
			undeployIndices:    []int{0},
			staterootsToRemove: []string{"rhcos_4.10.11"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			ostreeclientMock := ostreeclient.NewMockIClient(ctrl)
			rpmostreeclientMock := rpmostreeclient.NewMockIClient(ctrl)
			executorMock := ops.NewMockExecute(ctrl)

			rpmostreeclientMock.EXPECT().QueryStatus().Return(&rpmostreeclient.Status{
				Deployments: tt.deployments}, nil)
			for _, x := range tt.undeployIndices {
				ostreeclientMock.EXPECT().Undeploy(x)
			}
			for _, stateroot := range tt.staterootsToRemove {
				rpmostreeclientMock.EXPECT().QueryStatus().Return(&rpmostreeclient.Status{
					Deployments: tt.deployments}, nil)
				executorMock.EXPECT().Execute("unshare", "-m", "/bin/sh", "-c",
					fmt.Sprintf("\"mount -o remount,rw /sysroot && rm -rf /ostree/deploy/%s\"",
						stateroot))
			}
			r := &ImageBasedUpgradeReconciler{
				Log:             logr.Discard(),
				RPMOstreeClient: rpmostreeclientMock,
				Executor:        executorMock,
				OstreeClient:    ostreeclientMock,
			}
			osStat = func(name string) (os.FileInfo, error) {
				return os.Stat(".")
			}

			if err := r.cleanupUnbootedStateroots(); (err != nil) != tt.wantErr {
				t.Errorf("ImageBasedUpgradeReconciler.cleanupUnbootedStateroots() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
