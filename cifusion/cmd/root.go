package cmd

import (
	"fmt"
	"os"

	"cifusion/validator"

	"github.com/spf13/cobra"
)

var (
	provider string
)

var rootCmd = &cobra.Command{
	Use:   "cifusion",
	Short: "Validate CI workflow YAML files against JSON schemas",
}

var validateCmd = &cobra.Command{
	Use:   "validate [workflow.yaml]",
	Short: "Validate a workflow file",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var p validator.CIProvider
		switch provider {
		case "github":
			p = validator.GitHubActions
		case "circleci":
			p = validator.CircleCI
		case "gitlab":
			p = validator.GitLabCI
		default:
			return fmt.Errorf("unknown provider: %s", provider)
		}

		errs, err := validator.ValidateWorkflowForProvider(args[0], p)
		if err != nil {
			return err
		}
		if len(errs) > 0 {
			fmt.Println("Validation failed:")
			for _, e := range errs {
				fmt.Println("-", e)
			}
			os.Exit(1)
		}
		fmt.Println("Workflow is valid.")
		return nil
	},
}

func Execute() error {
	validateCmd.Flags().StringVar(&provider, "provider", "", "CI provider (github, circleci, gitlab)")
	if err := validateCmd.MarkFlagRequired("provider"); err != nil {
		return err
	}
	rootCmd.AddCommand(validateCmd)
	return rootCmd.Execute()
}
