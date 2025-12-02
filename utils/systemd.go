package utils

import (
	"bytes"
	_ "embed"
	"fmt"
	"text/template"
)

const (
	// NodeIP configuration paths
	PrimaryIPPath       = "/run/nodeip-configuration/primary-ip"
	NodeipDefaultsPath  = "/etc/default/nodeip-configuration"
	NodeipRerunUnitPath = "/etc/systemd/system/sno-nodeip-rerun.service"
)

// NodeIPRerunServiceTemplateData represents the template data for systemd service generation
type NodeIPRerunServiceTemplateData struct {
	BaseIP  string
	HintSed string
}

//go:embed templates/sno-nodeip-rerun.service.tmpl
var nodeipRerunServiceTemplate string

// GenerateNodeIPRerunService generates systemd service content from the provided template data
func GenerateNodeIPRerunService(data *NodeIPRerunServiceTemplateData) (string, error) {
	if err := validateNodeIPRerunServiceData(data); err != nil {
		return "", err
	}

	tmpl, err := template.New("nodeip-rerun-service").Parse(nodeipRerunServiceTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse systemd service template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute systemd service template: %w", err)
	}

	return buf.String(), nil
}

// validateNodeIPRerunServiceData validates the systemd service template data
func validateNodeIPRerunServiceData(data *NodeIPRerunServiceTemplateData) error {
	if data == nil {
		return fmt.Errorf("template data cannot be nil")
	}
	if data.BaseIP == "" {
		return fmt.Errorf("base IP is required")
	}
	if data.HintSed == "" {
		return fmt.Errorf("hint sed is required")
	}
	return nil
}
