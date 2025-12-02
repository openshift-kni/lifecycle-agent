package utils

import (
	"bytes"
	_ "embed"
	"fmt"
	"text/template"
)

// IgnitionNMStateTemplateData represents the template data for the ignition config
type IgnitionNMStateTemplateData struct {
	EncodedContent string
}

//go:embed templates/ignition-nmstate.json.tmpl
var ignitionNMStateTemplate string

// GenerateIgnitionNMState renders the ignition JSON for nmstate with the provided data
func GenerateIgnitionNMState(data *IgnitionNMStateTemplateData) (string, error) {
	if data == nil || data.EncodedContent == "" {
		return "", fmt.Errorf("encoded content is required")
	}

	tmpl, err := template.New("ignition-nmstate").Parse(ignitionNMStateTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse ignition template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute ignition template: %w", err)
	}

	return buf.String(), nil
}

// IgnitionDNSMasqFilterTemplateData represents the template data for the dnsmasq filter ignition config
type IgnitionDNSMasqFilterTemplateData struct {
	EncodedContent string
}

//go:embed templates/ignition-dnsmasq-filter.json.tmpl
var ignitionDNSMasqFilterTemplate string

// GenerateIgnitionDNSMasqFilter renders the ignition JSON for dnsmasq filter file with the provided data
func GenerateIgnitionDNSMasqFilter(data *IgnitionDNSMasqFilterTemplateData) (string, error) {
	if data == nil || data.EncodedContent == "" {
		return "", fmt.Errorf("encoded content is required")
	}

	tmpl, err := template.New("ignition-dnsmasq-filter").Parse(ignitionDNSMasqFilterTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse ignition template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute ignition template: %w", err)
	}

	return buf.String(), nil
}
