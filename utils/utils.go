package utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"text/template"
)

// WriteToFile write interface to file
func WriteToFile(data interface{}, filePath string) error {
	marshaled, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return os.WriteFile(filePath, marshaled, 0o644)
}

// RenderTemplateFile render template file
func RenderTemplateFile(srcTemplate string, params map[string]any, dest string, perm os.FileMode) error {
	templateData, err := os.ReadFile(srcTemplate)
	if err != nil {
		return fmt.Errorf("error occurred while trying to read %s: %w", srcTemplate, err)
	}

	tmpl := template.New("template")
	tmpl = template.Must(tmpl.Parse(string(templateData)))
	var buf bytes.Buffer
	if err = tmpl.Execute(&buf, params); err != nil {
		return fmt.Errorf("failed to render controller template: %w", err)
	}

	if err = os.WriteFile(dest, buf.Bytes(), perm); err != nil {
		return fmt.Errorf("error occurred while trying to write rendered data to %s: %w", dest, err)
	}
	return nil
}
