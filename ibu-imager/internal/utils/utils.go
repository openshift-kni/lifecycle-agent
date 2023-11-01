package utils

import (
	"bytes"
	"fmt"
	"os"
	"text/template"
)

func RenderTemplateFile(srcTemplate string, params map[string]interface{}, dest string, perm os.FileMode, delims []string) error {
	templateData, err := os.ReadFile(srcTemplate)
	if err != nil {
		return fmt.Errorf("error occurred while trying to read %s : %e", srcTemplate, err)
	}

	tmpl := template.New("template")
	if len(delims) > 1 {
		tmpl.Delims(delims[0], delims[1])
	}
	tmpl = template.Must(tmpl.Parse(string(templateData)))
	var buf bytes.Buffer
	if err = tmpl.Execute(&buf, params); err != nil {
		return fmt.Errorf("failed to render controller template: %e", err)
	}

	if err = os.WriteFile(dest, buf.Bytes(), perm); err != nil {
		return fmt.Errorf("error occurred while trying to write rendered data to %s : %e", dest, err)
	}
	return nil
}
