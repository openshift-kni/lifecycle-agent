package lcacli

import (
	"embed"
	"io/fs"
)

//go:embed installation_configuration_files
var installationConfigurationFS embed.FS

var InstallationConfigurationServices fs.FS

func init() {
	var err error
	InstallationConfigurationServices, err = fs.Sub(installationConfigurationFS, "installation_configuration_files/services")
	if err != nil {
		panic(err)
	}
}
