package controllers

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

func TestGetSeedImageMountpoint(t *testing.T) {
	testcases := []struct {
		name      string
		expect    string
		seedImage string
		output    string
	}{
		{
			name:      "",
			expect:    "/var/lib/containers/storage/overlay/56ea997c7c7aaf65c5faf125cac71933d4ad1aba1dfe5e1315d644495247f77a/merged",
			seedImage: "quay.io/saskari/seed:latest",
			output: `[
 {
  "id": "31ad66060775bf186a527f4fd1516e5b51fa3a6ac35590e3a6e7d1dfbfad0969",
  "Names": [
   "sha256:a9294c403e721dfc98d5b39f17cec0f195b62e8633ddd9814a13bd7dee0326e5"
  ],
  "Repositories": [],
  "mountpoint": "/var/lib/containers/storage/overlay/4fb5a0e4cd60f06dcbe801b492d9063fc6c8385b62632085d4477d89361f313a/merged"
 },
 {
  "id": "6959a6d0e5778a869b06e83edcb134747c4273f6dc916a3ba06289337dca675f",
  "Names": [
   "sha256:774346d4d3ecced8d31a512ec0f49c818c6b57854b694a5e68f72c7343cc95f3"
  ],
  "Repositories": [
   "quay.io/saskari/seed:latest"
  ],
  "mountpoint": "/var/lib/containers/storage/overlay/56ea997c7c7aaf65c5faf125cac71933d4ad1aba1dfe5e1315d644495247f77a/merged"
 }
]
`,
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := getSeedImageMountpoint(tc.seedImage, tc.output)
			assert.Equal(t, tc.expect, out)
			assert.NoError(t, err)
		})
	}
}
