package validator

import (
	"encoding/json"
	"errors"
	"os"

	"cifusion/schema"

	"gopkg.in/yaml.v3"
)

type CIProvider string

const (
	GitHubActions CIProvider = "github"
	CircleCI      CIProvider = "circleci"
	GitLabCI      CIProvider = "gitlab"
)

func GetSchemaURL(provider CIProvider) (string, error) {
	switch provider {
	case GitHubActions:
		return schema.GitHubActionsWorkflowSchema, nil
	case CircleCI:
		return schema.CircleCIWorkflowSchema, nil
	case GitLabCI:
		return schema.GitLabCIWorkflowSchema, nil
	default:
		return "", errors.New("unknown CI provider")
	}
}

func ValidateWorkflow(yamlPath string, schemaURL string) ([]error, error) {
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, err
	}

	var intermediate interface{}
	if err := yaml.Unmarshal(data, &intermediate); err != nil {
		return nil, err
	}

	jsonData, err := json.Marshal(intermediate)
	if err != nil {
		return nil, err
	}

	rs, err := schema.LoadSchema(schemaURL)
	if err != nil {
		return nil, err
	}

	valErrs, err := rs.ValidateBytes(jsonData)
	if err != nil {
		return nil, err
	}

	var errs []error
	for _, ve := range valErrs {
		errs = append(errs, ve)
	}

	return errs, nil
}

func ValidateWorkflowForProvider(yamlPath string, provider CIProvider) ([]error, error) {
	url, err := GetSchemaURL(provider)
	if err != nil {
		return nil, err
	}
	return ValidateWorkflow(yamlPath, url)
}
