package utils

import (
	"encoding/json"
	"os"
)

// WriteToFile write interface to file
func WriteToFile(data interface{}, filePath string) error {
	marshaled, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return os.WriteFile(filePath, marshaled, 0o644)
}
