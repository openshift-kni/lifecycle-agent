package utils

import (
	"path/filepath"
)

const (
	IBUWorkspacePath string = "/var/ibu"
	// IBUName defines the valid name of the CR for the controller to reconcile
	IBUName     string = "upgrade"
	IBUFilePath string = "/opt/ibu.json"

	ManualCleanupAnnotation string = "lca.openshift.io/manualCleanupDone"

	// SeedGenName defines the valid name of the CR for the controller to reconcile
	SeedGenName          string = "seedimage"
	SeedGenSecretName    string = "seedgen"
	SeedgenWorkspacePath string = "/var/tmp/ibu-seedgen-orch" // The /var/tmp folder is excluded from the var.tgz backup in seed image creation
)

var (
	SeedGenStoredCR       = filepath.Join(SeedgenWorkspacePath, "seedgen-cr.json")
	SeedGenStoredSecretCR = filepath.Join(SeedgenWorkspacePath, "seedgen-secret.json")
)
