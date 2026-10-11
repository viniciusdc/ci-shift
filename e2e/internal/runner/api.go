package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// api is a minimal JSON REST client.
type api struct {
	base   string // scheme://host[/prefix], no trailing slash
	auth   func(*http.Request)
	client *http.Client
}

func newAPI(base string, auth func(*http.Request)) *api {
	return &api{base: strings.TrimRight(base, "/"), auth: auth, client: &http.Client{Timeout: 60 * time.Second}}
}

// httpError is a non-2xx answer. Its message carries the start of the
// response body but never the request, so credentials cannot leak.
type httpError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

func (e *httpError) Is(target error) bool {
	return target == errNotFound && e.Status == http.StatusNotFound
}

// do sends a request with an optional JSON body and decodes a JSON answer
// into out (when not nil). path is appended to the base URL.
func (a *api) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.auth != nil {
		a.auth(req)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		// url.Error includes the URL, which never carries credentials here.
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		p := path
		if i := strings.IndexByte(p, '?'); i >= 0 {
			p = p[:i]
		}
		return &httpError{Method: method, Path: p, Status: resp.StatusCode, Body: msg}
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: decode: %w", method, path, err)
	}
	return nil
}

func basicAuth(user, password string) func(*http.Request) {
	return func(r *http.Request) { r.SetBasicAuth(user, password) }
}

func bearerAuth(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func headerAuth(name, value string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(name, value) }
}

// basicHeader is the value of an HTTP Basic Authorization header.
func basicHeader(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}
