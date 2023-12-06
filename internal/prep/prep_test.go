package prep

import (
	"log"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

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
				"--karg-append", "tsc=nowatchdog",
				"--karg-append", "nosoftlockup",
				"--karg-append", "nmi_watchdog=0",
				"--karg-append", "mce=off",
				"--karg-append", "rcutree.kthread_prio=11",
				"--karg-append", "default_hugepagesz=1G",
				"--karg-append", "hugepagesz=1G",
				"--karg-append", "hugepages=32",
				"--karg-append", "rcupdate.rcu_normal_after_boot=0",
				"--karg-append", "efi=runtime",
				"--karg-append", "vfio_pci.enable_sriov=1",
				"--karg-append", "vfio_pci.disable_idle_d3=1",
				"--karg-append", "module_blacklist=irdma",
				"--karg-append", "intel_pstate=disable"},
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

			res, err := BuildKernelArgumentsFromMCOFile(f.Name())
			assert.Equal(t, tc.expect, res)
			assert.NoError(t, err)
		})
	}
}
