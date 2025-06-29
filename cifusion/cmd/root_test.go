package cmd

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCifusionCLI_ValidWorkflow(t *testing.T) {
	cmd := exec.Command("../cifusion", "validate", "--provider", "github", "../tests/files/valid_workflows/github.yaml")
	output, err := cmd.CombinedOutput()
	assert.NoError(t, err)
	assert.Contains(t, string(output), "Workflow is valid.")
}

func TestCifusionCLI_InvalidWorkflow(t *testing.T) {
	cmd := exec.Command("../cifusion", "validate", "--provider", "github", "../tests/files/invalid_workflows/github.yaml")
	output, err := cmd.CombinedOutput()
	assert.Error(t, err, "Expected non-zero exit for invalid workflow")
	assert.Contains(t, string(output), "Validation failed:")
}
