package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGenerateNodeIPRerunService_Success(t *testing.T) {
	data := &NodeIPRerunServiceTemplateData{
		BaseIP:  "192.168.1.0",
		HintSed: "192.168.1.0",
	}

	unit, err := GenerateNodeIPRerunService(data)
	assert.NoError(t, err)
	assert.NotEmpty(t, unit)

	// The rendered unit should contain both the base IP and the sed‑escaped hint
	assert.Contains(t, unit, "192.168.1.0")
}

func TestGenerateNodeIPRerunService_NilData(t *testing.T) {
	unit, err := GenerateNodeIPRerunService(nil)
	assert.Error(t, err)
	assert.Equal(t, "", unit)
}

func TestGenerateNodeIPRerunService_MissingBaseIP(t *testing.T) {
	data := &NodeIPRerunServiceTemplateData{
		BaseIP:  "",
		HintSed: "hint",
	}

	unit, err := GenerateNodeIPRerunService(data)
	assert.Error(t, err)
	assert.Equal(t, "", unit)
}

func TestGenerateNodeIPRerunService_MissingHintSed(t *testing.T) {
	data := &NodeIPRerunServiceTemplateData{
		BaseIP:  "192.168.1.0",
		HintSed: "",
	}

	unit, err := GenerateNodeIPRerunService(data)
	assert.Error(t, err)
	assert.Equal(t, "", unit)
}
