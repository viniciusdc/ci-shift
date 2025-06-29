package schema

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/qri-io/jsonschema"
)

var (
	schemaCacheDir string
	schemaOnce     sync.Once
)

func setCacheDir() {
	home, err := os.UserCacheDir()
	if err != nil {
		home = os.TempDir()
	}
	schemaCacheDir = filepath.Join(home, "ci-schema-cache")
	_ = os.MkdirAll(schemaCacheDir, 0o755)
}

func LoadSchema(url string) (*jsonschema.RootSchema, error) {
	schemaOnce.Do(setCacheDir)

	name := filepath.Base(url)
	cachedPath := filepath.Join(schemaCacheDir, name)

	var data []byte
	var err error

	if _, err = os.Stat(cachedPath); errors.Is(err, os.ErrNotExist) {
		resp, err := http.Get(url)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, errors.New("failed to download schema: " + resp.Status)
		}
		data, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(cachedPath, data, 0o644); err != nil {
			return nil, err
		}
	} else {
		data, err = os.ReadFile(cachedPath)
		if err != nil {
			return nil, err
		}
	}

	rs := &jsonschema.RootSchema{}
	if err := rs.UnmarshalJSON(data); err != nil {
		return nil, err
	}

	return rs, nil
}
