package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOperatorNamespace(t *testing.T) {
	t.Setenv(OperatorNamespaceEnvVar, "")
	assert.Equal(t, DefaultOperatorNamespace, OperatorNamespace())

	t.Setenv(OperatorNamespaceEnvVar, "custom-operator-ns")
	assert.Equal(t, "custom-operator-ns", OperatorNamespace())
}
