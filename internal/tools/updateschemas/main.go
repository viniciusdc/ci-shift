// Command updateschemas refreshes the schema snapshots embedded in the
// schema package from their upstream URLs.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/viniciusdc/cistela/schema"
)

func main() {
	client := &http.Client{Timeout: 2 * time.Minute}
	failed := false
	for _, src := range schema.Sources {
		if err := fetch(client, src); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", src.Provider, err)
			failed = true
			continue
		}
		fmt.Printf("%s: updated\n", src.Provider)
	}
	if failed {
		os.Exit(1)
	}
}

func fetch(client *http.Client, src schema.Source) error {
	resp, err := client.Get(src.URL)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", src.URL, resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if !json.Valid(data) {
		return fmt.Errorf("%s: not JSON", src.URL)
	}
	return os.WriteFile(filepath.Join("schema", "snapshots", src.Provider+".json"), data, 0o644)
}
