package prep

import (
	"fmt"
	"log"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetDeploymentFromDeploymentID(t *testing.T) {
	testcases := []struct {
		name         string
		deploymentID string
		expect       string
		err          error
	}{
		{
			name:         "normal version",
			deploymentID: "rhcos-ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1",
			expect:       "ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1",
			err:          nil,
		},
		{
			name:         "multiple dashes",
			deploymentID: "4.15.0-0.nightly-2023-12-04-223539-ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1",
			expect:       "ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1",
			err:          nil,
		},
		{
			name:         "no dashes",
			deploymentID: "ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1",
			expect:       "",
			err: fmt.Errorf(
				"failed to get deployment from deploymentID, there should be a '-' in deployment"),
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := getDeploymentFromDeploymentID(tc.deploymentID)
			assert.Equal(t, tc.expect, res)
			if tc.err != nil {
				assert.ErrorContains(t, err, tc.err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestGetKernelArgumentsFromMCOFile(t *testing.T) {
	testcases := []struct {
		name   string
		data   string
		expect []string
	}{
		{
			name: "multiple args",
			data: `{"spec":{"kernelArguments":[    "tsc=nowatchdog",
			"nosoftlockup",                                                                                                                     
            "cgroup_no_v1=\"all\"",
			"nmi_watchdog=0",                                                                                                                   
			"mce=off",                                                                                                                          
			"rcutree.kthread_prio=11",
			"default_hugepagesz=1G",                                        
			"hugepagesz=1G",
			"hugepages=32",
			"rcupdate.rcu_normal_after_boot=0",                                                                                                 
			"efi=runtime",                                                                                                                      
			"vfio_pci.enable_sriov=1",                                                                                                          
			"vfio_pci.disable_idle_d3=1",                                                                                                       
			"module_blacklist=irdma",                                                                                                           
			"intel_pstate=disable"         ]}}`,
			expect: []string{
				"--karg-append", "\"tsc=nowatchdog\"",
				"--karg-append", "\"nosoftlockup\"",
				"--karg-append", "\"cgroup_no_v1=\\\"all\\\"\"",
				"--karg-append", "\"nmi_watchdog=0\"",
				"--karg-append", "\"mce=off\"",
				"--karg-append", "\"rcutree.kthread_prio=11\"",
				"--karg-append", "\"default_hugepagesz=1G\"",
				"--karg-append", "\"hugepagesz=1G\"",
				"--karg-append", "\"hugepages=32\"",
				"--karg-append", "\"rcupdate.rcu_normal_after_boot=0\"",
				"--karg-append", "\"efi=runtime\"",
				"--karg-append", "\"vfio_pci.enable_sriov=1\"",
				"--karg-append", "\"vfio_pci.disable_idle_d3=1\"",
				"--karg-append", "\"module_blacklist=irdma\"",
				"--karg-append", "\"intel_pstate=disable\"",
			},
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			// create fixture
			f, err := os.CreateTemp("", "tmp")
			if err != nil {
				log.Fatal(err)
			}
			defer os.Remove(f.Name())
			if _, err := f.Write([]byte(tc.data)); err != nil {
				log.Fatal(err)
			}
			if err := f.Close(); err != nil {
				log.Fatal(err)
			}

			res, err := buildKernelArgumentsFromMCOFile(f.Name())
			assert.Equal(t, tc.expect, res)
			assert.NoError(t, err)
		})
	}
}
