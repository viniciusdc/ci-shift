// Command cistela translates CI/CD pipelines between providers.
package main

import (
	"os"

	"github.com/viniciusdc/cistela/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
