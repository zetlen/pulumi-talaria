package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// client speaks the iac HTTP protocol of one Talaria instance.
type client struct {
	base, apiKey string
	http         *http.Client
}

func newClient(baseURL, apiKey string) *client {
	return &client{base: strings.TrimRight(baseURL, "/") + "/api/iac", apiKey: apiKey, http: &http.Client{Timeout: 60 * time.Second}}
}

// do sends one request; body and out may be nil. Non-2xx responses become errors carrying
// the server's `error` and `issues`.
func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "ApiKey "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return httpError(method, path, resp.StatusCode, data)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: decoding response: %w", method, path, err)
	}
	return nil
}

func httpError(method, path string, status int, data []byte) error {
	var e struct {
		Error  string          `json:"error"`
		Issues json.RawMessage `json:"issues"`
	}
	if json.Unmarshal(data, &e) != nil || e.Error == "" {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, status, strings.TrimSpace(string(data)))
	}
	if len(e.Issues) > 0 && string(e.Issues) != "null" {
		return fmt.Errorf("%s %s: HTTP %d: %s (issues: %s)", method, path, status, e.Error, e.Issues)
	}
	return fmt.Errorf("%s %s: HTTP %d: %s", method, path, status, e.Error)
}

func resourcePath(kind, key string) string {
	p := "/resources/" + url.PathEscape(kind)
	if key != "" {
		p += "/" + url.PathEscape(key)
	}
	return p
}

// get returns the state of key, or nil when it does not exist.
func (c *client) get(ctx context.Context, kind, key string) (map[string]any, error) {
	var out struct {
		Items map[string]map[string]any `json:"items"`
	}
	q := url.Values{"key": {key}}
	if err := c.do(ctx, http.MethodGet, resourcePath(kind, "")+"?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Items[key], nil
}

func (c *client) put(ctx context.Context, kind, key string, inputs map[string]any) (map[string]any, error) {
	var out map[string]any
	return out, c.do(ctx, http.MethodPut, resourcePath(kind, key), inputs, &out)
}

func (c *client) delete(ctx context.Context, kind, key string) error {
	return c.do(ctx, http.MethodDelete, resourcePath(kind, key), nil, nil)
}

func (c *client) data(ctx context.Context, kind string, args map[string]any) (map[string]any, error) {
	var out map[string]any
	return out, c.do(ctx, http.MethodPost, "/data/"+url.PathEscape(kind), args, &out)
}
