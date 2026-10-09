// Package fake is an in-memory implementation of Talaria's iac protocol (version 1) for the
// kinds auth.role_acl, api_keys.api_key and directory.scope. Tests and the e2e script use it.
package fake

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
)

// Kinds is the kinds document the fake serves.
//
//go:embed kinds.json
var Kinds []byte

const orgID = "11111111-1111-1111-1111-111111111111"

// Server implements the protocol; it is an http.Handler. Besides /api/iac it serves
// /_fake/{kind}/{key} for tests: GET dumps, PATCH merges JSON into the state, DELETE removes.
type Server struct {
	APIKey string

	mu    sync.Mutex
	state map[string]map[string]map[string]any // kind -> key -> state
}

// New returns a server that accepts the given API key and knows roles admin and employee.
func New(apiKey string) *Server {
	s := &Server{APIKey: apiKey, state: map[string]map[string]map[string]any{
		"auth.role_acl": {}, "api_keys.api_key": {},
	}}
	for _, r := range []string{"admin", "employee"} {
		s.state["auth.role_acl"][r] = map[string]any{"role": r, "features": []any{}, "isSuperAdmin": false, "organizations": nil}
	}
	return s
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func fail(w http.ResponseWriter, status int, msg string, issues ...any) {
	body := map[string]any{"error": msg}
	if len(issues) > 0 {
		body["issues"] = issues
	}
	reply(w, status, body)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rest, ok := strings.CutPrefix(r.URL.Path, "/_fake/"); ok {
		s.admin(w, r, rest)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/api/iac/")
	if !ok {
		fail(w, 404, "not found")
		return
	}
	if r.Header.Get("Authorization") != "ApiKey "+s.APIKey {
		fail(w, 401, "Unauthorized")
		return
	}
	parts := strings.Split(rest, "/") // kinds | resources/{kind}[/{key}] | data/{kind}
	for i, p := range parts {
		parts[i], _ = url.PathUnescape(p) // keys may contain %2F, so split before unescaping
	}
	switch {
	case rest == "kinds" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(Kinds)
	case parts[0] == "data" && len(parts) == 2 && r.Method == http.MethodPost:
		if parts[1] != "directory.scope" {
			fail(w, 404, "unknown kind")
			return
		}
		reply(w, 200, map[string]any{"tenantId": "22222222-2222-2222-2222-222222222222", "tenantName": "Fake Tenant", "organizationId": orgID, "organizationName": "Fake Org"})
	case parts[0] == "resources" && len(parts) >= 2:
		s.resources(w, r, parts)
	default:
		fail(w, 404, "not found")
	}
}

func (s *Server) resources(w http.ResponseWriter, r *http.Request, parts []string) {
	kind := parts[1]
	store, ok := s.state[kind]
	if !ok {
		fail(w, 404, "unknown kind")
		return
	}
	switch {
	case len(parts) == 2 && r.Method == http.MethodGet:
		items := map[string]any{}
		for _, k := range r.URL.Query()["key"] {
			if st, ok := store[k]; ok {
				items[k] = view(kind, st)
			} else {
				items[k] = nil
			}
		}
		reply(w, 200, map[string]any{"items": items})
	case len(parts) == 3 && r.Method == http.MethodPut:
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, 400, "invalid body")
			return
		}
		st, status, msg := put(kind, store, parts[2], body)
		if msg != "" {
			fail(w, status, msg, map[string]any{"message": msg})
			return
		}
		reply(w, 200, st)
	case len(parts) == 3 && r.Method == http.MethodDelete:
		key := parts[2]
		if kind == "auth.role_acl" {
			if _, ok := store[key]; ok {
				store[key] = map[string]any{"role": key, "features": []any{}, "isSuperAdmin": false, "organizations": nil}
			}
		} else {
			delete(store, key)
		}
		reply(w, 204, nil)
	default:
		fail(w, 404, "not found")
	}
}

// view is what GET returns: the stored state, minus secrets (shown only by the creating put).
func view(kind string, st map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range st {
		out[k] = v
	}
	if kind == "api_keys.api_key" {
		delete(out, "secret")
	}
	return out
}

func put(kind string, store map[string]map[string]any, key string, body map[string]any) (map[string]any, int, string) {
	keyField := map[string]string{"auth.role_acl": "role", "api_keys.api_key": "name"}[kind]
	if body[keyField] != key {
		return nil, 400, "key mismatch: body." + keyField + " must equal the path key"
	}
	switch kind {
	case "auth.role_acl":
		cur, ok := store[key]
		if !ok {
			return nil, 400, "role does not exist: " + key
		}
		if _, ok := body["features"].([]any); !ok {
			return nil, 400, "features must be an array"
		}
		cur["features"] = body["features"]
		cur["isSuperAdmin"] = body["isSuperAdmin"] == true
		cur["organizations"] = body["organizations"]
		return view(kind, cur), 0, ""
	default: // api_keys.api_key
		if _, ok := body["roles"].([]any); !ok {
			return nil, 400, "roles must be an array"
		}
		if body["organizationId"] == nil {
			body["organizationId"] = orgID
		}
		if cur, ok := store[key]; ok {
			for _, f := range []string{"roles", "organizationId", "expiresAt"} {
				if !reflect.DeepEqual(cur[f], body[f]) {
					return nil, 400, "api key is immutable: cannot change " + f
				}
			}
			cur["description"] = body["description"]
			return view(kind, cur), 0, ""
		}
		id, prefix, secret := randHex(16), "omk_"+randHex(4), "omk_"+randHex(24)
		st := map[string]any{
			"name": key, "description": body["description"], "roles": body["roles"], "organizationId": body["organizationId"],
			"expiresAt": body["expiresAt"], "id": id[:8] + "-0000-0000-0000-" + id[8:20], "keyPrefix": prefix, "secret": secret,
		}
		store[key] = st
		return st, 0, "" // the creating put returns the secret
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) admin(w http.ResponseWriter, r *http.Request, rest string) {
	kind, key, _ := strings.Cut(rest, "/")
	store, ok := s.state[kind]
	if !ok {
		fail(w, 404, "unknown kind")
		return
	}
	switch r.Method {
	case http.MethodGet:
		reply(w, 200, store[key])
	case http.MethodPatch:
		var patch map[string]any
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			fail(w, 400, "invalid body")
			return
		}
		if store[key] == nil {
			store[key] = map[string]any{}
		}
		for k, v := range patch {
			store[key][k] = v
		}
		reply(w, 200, store[key])
	case http.MethodDelete:
		delete(store, key)
		reply(w, 204, nil)
	}
}
