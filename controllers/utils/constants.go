package utils

// IBUName defines the valid name of the CR for the controller to reconcile
const (
	IBUWorkspacePath   string = "/var/ibu"
	Host               string = "/host"
	PrepGetSeedImage   string = "prepGetSeedImage.sh"
	PrepSetupStateroot string = "prepSetupStateroot.sh"
	PrepCleanup        string = "prepCleanup.sh"
	IBUName                   = "upgrade"
)
