package validator

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateWorkflowForProvider_Valid(t *testing.T) {
	errs, err := ValidateWorkflowForProvider("../tests/files/valid_workflows/github.yaml", GitHubActions)
	assert.NoError(t, err)
	assert.Len(t, errs, 0, "Expected no validation errors for valid workflow")
}

func TestValidateWorkflowForProvider_Invalid(t *testing.T) {
	errs, err := ValidateWorkflowForProvider("../tests/files/invalid_workflows/github.yaml", GitHubActions)
	assert.NoError(t, err)
	assert.NotEmpty(t, errs, "Expected validation errors for invalid workflow")
	for _, e := range errs {
		t.Logf("Validation error: %s", e.Error())
	}
}

func TestGetSchemaURL_InvalidProvider(t *testing.T) {
	_, err := GetSchemaURL(CIProvider("unknown"))
	assert.Error(t, err)
}
