package lcacli

import _ "embed"

//go:embed installation_configuration_files/services/lca-init-monitor.service
var LcaInitMonitorServiceFile string

//go:embed ip_configuration_files/services/ip-configuration.service
var IpConfigurationServiceFile string
