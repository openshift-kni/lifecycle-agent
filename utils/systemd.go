package utils

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"strings"
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

// CreateNodeIPRerunService creates the systemd nodeip rerun service with default paths
// and the provided machine network CIDR (e.g., "192.168.1.0/24")
func CreateNodeIPRerunService(machineNetwork string) error {
	// Extract base IP from machine network CIDR
	baseIP := strings.Split(machineNetwork, "/")[0]
	hintSed := strings.ReplaceAll(baseIP, "/", "\\/")

	// Create template data
	templateData := &NodeIPRerunServiceTemplateData{
		BaseIP:  baseIP,
		HintSed: hintSed,
	}

	// Generate the systemd unit file content using the template
	unitContent, err := GenerateNodeIPRerunService(templateData)
	if err != nil {
		return fmt.Errorf("failed to generate nodeip rerun service content: %w", err)
	}

	// Write the service file
	if err := os.WriteFile(NodeipRerunUnitPath, []byte(unitContent), 0644); err != nil {
		return fmt.Errorf("failed to write nodeip rerun service file: %w", err)
	}

	return nil
}
