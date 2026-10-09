package provider_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blang/semver"
	presource "github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	p "github.com/pulumi/pulumi-go-provider"

	"github.com/zetlen/pulumi-talaria/internal/fake"
	"github.com/zetlen/pulumi-talaria/provider"
)

var (
	ctx     = context.Background()
	version = semver.MustParse("1.2.3")
)

// val builds a property value from plain Go values.
func val(x any) property.Value {
	switch x := x.(type) {
	case nil:
		return property.Value{}
	case string:
		return property.New(x)
	case bool:
		return property.New(x)
	case int:
		return property.New(float64(x))
	case []string:
		arr := make([]property.Value, len(x))
		for i, s := range x {
			arr[i] = property.New(s)
		}
		return property.New(arr)
	case property.Value:
		return x
	}
	panic("val: unsupported type")
}

func pm(m map[string]any) property.Map {
	out := map[string]property.Value{}
	for k, v := range m {
		out[k] = val(v)
	}
	return property.NewMap(out)
}

func urn(token string) presource.URN {
	return presource.NewURN("stack", "proj", "", tokens.Type(token), "r")
}

const (
	aclTok = "talaria:auth:RoleAcl"
	keyTok = "talaria:api_keys:ApiKey"
)

type env struct {
	t    *testing.T
	prov p.Provider
	srv  *httptest.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	srv := httptest.NewServer(fake.New("k"))
	t.Cleanup(srv.Close)
	prov := provider.New(version)
	_, err := prov.Parameterize(ctx, p.ParameterizeRequest{Value: &p.ParameterizeRequestValue{Name: "talaria", Version: version, Value: fake.Kinds}})
	require.NoError(t, err)
	require.NoError(t, prov.Configure(ctx, p.ConfigureRequest{Args: pm(map[string]any{"url": srv.URL, "apiKey": val("k").WithSecret(true)})}))
	return &env{t: t, prov: prov, srv: srv}
}

// server sends a raw request to the fake (its /_fake endpoints mutate state behind the provider's back).
func (e *env) server(method, path, body string) string {
	req, err := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	require.NoError(e.t, err)
	req.Header.Set("Authorization", "ApiKey k")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(e.t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(e.t, err)
	return string(b)
}

func (e *env) check(tok string, in property.Map) property.Map {
	e.t.Helper()
	r, err := e.prov.Check(ctx, p.CheckRequest{Urn: urn(tok), Inputs: in})
	require.NoError(e.t, err)
	require.Empty(e.t, r.Failures)
	return r.Inputs
}

func (e *env) create(tok string, in property.Map) p.CreateResponse {
	e.t.Helper()
	r, err := e.prov.Create(ctx, p.CreateRequest{Urn: urn(tok), Properties: e.check(tok, in)})
	require.NoError(e.t, err)
	return r
}

func str(t *testing.T, m property.Map, k string) string {
	t.Helper()
	v, ok := m.GetOk(k)
	require.True(t, ok, "missing %q in %v", k, m)
	require.True(t, v.IsString(), "%q is not a string", k)
	return v.AsString()
}

func TestParameterize(t *testing.T) {
	prov := provider.New(version)
	args := func(a ...string) p.ParameterizeRequest {
		return p.ParameterizeRequest{Args: &p.ParameterizeRequestArgs{Args: a}}
	}

	t.Run("file path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "kinds.json")
		require.NoError(t, os.WriteFile(path, fake.Kinds, 0o600))
		resp, err := prov.Parameterize(ctx, args(path))
		require.NoError(t, err)
		assert.Equal(t, "talaria", resp.Name)
		assert.Equal(t, version, resp.Version)
		schema, err := prov.GetSchema(ctx, p.GetSchemaRequest{})
		require.NoError(t, err)
		assert.Contains(t, schema.Schema, "talaria:auth:RoleAcl")
	})

	t.Run("URL with api key", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			_, _ = w.Write(fake.Kinds)
		}))
		defer srv.Close()
		t.Setenv("TALARIA_API_KEY", "sekret")
		_, err := provider.New(version).Parameterize(ctx, args(srv.URL+"/api/iac/kinds"))
		require.NoError(t, err)
		assert.Equal(t, "ApiKey sekret", gotAuth)
	})

	t.Run("URL error surfaces the server message", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"Forbidden"}`))
		}))
		defer srv.Close()
		_, err := provider.New(version).Parameterize(ctx, args(srv.URL))
		assert.ErrorContains(t, err, "Forbidden")
	})

	t.Run("embedded value", func(t *testing.T) {
		pr := provider.New(version)
		_, err := pr.Parameterize(ctx, p.ParameterizeRequest{Value: &p.ParameterizeRequestValue{Name: "talaria", Version: version, Value: fake.Kinds}})
		require.NoError(t, err)
		schema, err := pr.GetSchema(ctx, p.GetSchemaRequest{})
		require.NoError(t, err)
		assert.Contains(t, schema.Schema, "talaria:directory:getScope")
	})

	t.Run("rejects bad input", func(t *testing.T) {
		_, err := prov.Parameterize(ctx, args())
		assert.ErrorContains(t, err, "exactly one argument")
		_, err = prov.Parameterize(ctx, args("a", "b"))
		assert.ErrorContains(t, err, "exactly one argument")
		_, err = prov.Parameterize(ctx, p.ParameterizeRequest{Value: &p.ParameterizeRequestValue{Value: []byte(`{"protocol":2,"kinds":[]}`)}})
		assert.ErrorContains(t, err, "unsupported kinds protocol 2")
		_, err = prov.Parameterize(ctx, p.ParameterizeRequest{})
		assert.Error(t, err)
	})
}

func TestBaseProviderSchema(t *testing.T) {
	schema, err := provider.New(version).GetSchema(ctx, p.GetSchemaRequest{})
	require.NoError(t, err)
	assert.NotContains(t, schema.Schema, "parameterization")
}

func TestConfigure(t *testing.T) {
	prov := provider.New(version)
	t.Setenv("TALARIA_URL", "")
	assert.ErrorContains(t, prov.Configure(ctx, p.ConfigureRequest{}), "url is required")

	srv := httptest.NewServer(fake.New("envkey"))
	defer srv.Close()
	t.Setenv("TALARIA_URL", srv.URL)
	t.Setenv("TALARIA_API_KEY", "envkey")
	require.NoError(t, prov.Configure(ctx, p.ConfigureRequest{}))
	_, err := prov.Parameterize(ctx, p.ParameterizeRequest{Value: &p.ParameterizeRequestValue{Version: version, Value: fake.Kinds}})
	require.NoError(t, err)
	out, err := prov.Invoke(ctx, p.InvokeRequest{Token: "talaria:directory:getScope"})
	require.NoError(t, err)
	assert.Equal(t, "Fake Org", str(t, out.Return, "organizationName"))
}

func TestCheck(t *testing.T) {
	e := newEnv(t)

	in := e.check(aclTok, pm(map[string]any{"role": "admin", "features": []string{"a.b"}}))
	assert.Equal(t, false, in.Get("isSuperAdmin").AsBool(), "schema default applied")
	assert.Equal(t, "admin", str(t, in, "role"), "inputs kept")
	_, hasOrgs := in.GetOk("organizations")
	assert.False(t, hasOrgs, "no default for nullable field")

	r, err := e.prov.Check(ctx, p.CheckRequest{Urn: urn(keyTok), Inputs: pm(map[string]any{"name": "ci"})})
	require.NoError(t, err)
	require.Len(t, r.Failures, 1)
	assert.Equal(t, "roles", r.Failures[0].Property)

	r, err = e.prov.Check(ctx, p.CheckRequest{Urn: urn(keyTok), Inputs: pm(map[string]any{"name": 3, "roles": []string{}})})
	require.NoError(t, err)
	require.Len(t, r.Failures, 1)
	assert.Equal(t, "name", r.Failures[0].Property)

	_, err = e.prov.Check(ctx, p.CheckRequest{Urn: urn("talaria:auth:Nope")})
	assert.ErrorContains(t, err, "unknown token")
}

func TestApiKeyLifecycle(t *testing.T) {
	e := newEnv(t)
	inputs := pm(map[string]any{"name": "ci", "roles": []string{"admin"}, "description": "first"})

	// preview
	dry, err := e.prov.Create(ctx, p.CreateRequest{Urn: urn(keyTok), Properties: e.check(keyTok, inputs), DryRun: true})
	require.NoError(t, err)
	assert.Empty(t, dry.ID)
	assert.True(t, dry.Properties.Get("secret").IsComputed())
	assert.True(t, dry.Properties.Get("apiKeyId").IsComputed())
	assert.Equal(t, `{"items":{"ci":null}}`+"\n", e.server("GET", "/api/iac/resources/api_keys.api_key?key=ci", ""), "dry run must not write")

	// create
	created := e.create(keyTok, inputs)
	assert.Equal(t, "ci", created.ID, "ID = key")
	secret := str(t, created.Properties, "secret")
	assert.True(t, strings.HasPrefix(secret, "omk_"))
	assert.True(t, created.Properties.Get("secret").Secret(), "secretFields are secret in state")
	assert.False(t, created.Properties.Get("keyPrefix").Secret())
	assert.NotEmpty(t, str(t, created.Properties, "apiKeyId"), "protocol `id` is exposed as apiKeyId")
	_, hasID := created.Properties.GetOk("id")
	assert.False(t, hasID)
	assert.Equal(t, "first", str(t, created.Properties, "description"))

	// read (refresh): server no longer returns the secret -> keep the old one
	checked := e.check(keyTok, inputs)
	rd, err := e.prov.Read(ctx, p.ReadRequest{ID: "ci", Urn: urn(keyTok), Properties: created.Properties, Inputs: checked})
	require.NoError(t, err)
	assert.Equal(t, "ci", rd.ID)
	assert.Equal(t, secret, str(t, rd.Properties, "secret"), "omitted output keeps its last known value")
	assert.True(t, rd.Properties.Get("secret").Secret())
	assert.Equal(t, str(t, created.Properties, "apiKeyId"), str(t, rd.Properties, "apiKeyId"))
	for _, out := range []string{"secret", "apiKeyId", "keyPrefix", "id"} {
		_, isInput := rd.Inputs.GetOk(out)
		assert.False(t, isInput, "read inputs hold only input fields, found %q", out)
	}
	assert.Equal(t, "first", str(t, rd.Inputs, "description"))
	_, hasOrg := rd.Inputs.GetOk("organizationId")
	assert.False(t, hasOrg, "server-defaulted optional input the program never set stays out of the inputs on refresh")
	assert.NotEmpty(t, str(t, rd.Properties, "organizationId"), "but it is in the state")

	// import: no previous inputs -> everything the server has is an input
	imp, err := e.prov.Read(ctx, p.ReadRequest{ID: "ci", Urn: urn(keyTok)})
	require.NoError(t, err)
	assert.NotEmpty(t, str(t, imp.Inputs, "organizationId"))
	_, hasSecret := imp.Properties.GetOk("secret")
	assert.False(t, hasSecret, "nothing to preserve on import")

	// diff: nothing changed
	diff := func(old, new property.Map) p.DiffResponse {
		r, err := e.prov.Diff(ctx, p.DiffRequest{ID: "ci", Urn: urn(keyTok), State: created.Properties, OldInputs: old, Inputs: new})
		require.NoError(t, err)
		return r
	}
	assert.False(t, diff(checked, checked).HasChanges)

	// diff: description is an in-place update; removing it is a delete, still in place
	upd := e.check(keyTok, pm(map[string]any{"name": "ci", "roles": []string{"admin"}, "description": "second"}))
	d := diff(checked, upd)
	assert.True(t, d.HasChanges)
	assert.True(t, d.DeleteBeforeReplace, "key unchanged: delete first, whatever changed")
	assert.False(t, diff(checked, checked).HasChanges)
	assert.True(t, diff(checked, checked).DeleteBeforeReplace, "key unchanged and nothing changed: still delete first for forced replaces")
	assert.Equal(t, map[string]p.PropertyDiff{"description": {Kind: p.Update, InputDiff: true}}, d.DetailedDiff)
	nodesc := e.check(keyTok, pm(map[string]any{"name": "ci", "roles": []string{"admin"}}))
	assert.Equal(t, p.Delete, diff(checked, nodesc).DetailedDiff["description"].Kind)
	assert.Equal(t, p.Add, diff(nodesc, checked).DetailedDiff["description"].Kind)
	// null and absent are the same
	withNull := e.check(keyTok, pm(map[string]any{"name": "ci", "roles": []string{"admin"}, "description": nil}))
	assert.False(t, diff(nodesc, withNull).HasChanges)
	// secret-ness of an input is not a change
	secretDesc := e.check(keyTok, pm(map[string]any{"name": "ci", "roles": []string{"admin"}, "description": val("first").WithSecret(true)}))
	assert.False(t, diff(checked, secretDesc).HasChanges)
	// unknown new value is a change
	unknown := e.check(keyTok, pm(map[string]any{"name": "ci", "roles": []string{"admin"}, "description": property.New(property.Computed)}))
	assert.True(t, diff(checked, unknown).HasChanges)

	// diff: replaceOnChanges field -> replace, delete before create
	roles := e.check(keyTok, pm(map[string]any{"name": "ci", "roles": []string{"admin", "employee"}, "description": "first"}))
	d = diff(checked, roles)
	assert.True(t, d.DeleteBeforeReplace)
	assert.Equal(t, map[string]p.PropertyDiff{"roles": {Kind: p.UpdateReplace, InputDiff: true}}, d.DetailedDiff)
	// diff: key field -> replace; adding a replace-field is add&replace
	renamed := e.check(keyTok, pm(map[string]any{"name": "ci2", "roles": []string{"admin"}, "description": "first", "expiresAt": "2030-01-01T00:00:00Z"}))
	d = diff(checked, renamed)
	assert.False(t, d.DeleteBeforeReplace, "new key coexists with the old object, whatever else changed")
	assert.Equal(t, p.UpdateReplace, d.DetailedDiff["name"].Kind)
	assert.Equal(t, p.AddReplace, d.DetailedDiff["expiresAt"].Kind)
	// key field alone: old and new can coexist, so create before delete
	onlyRenamed := e.check(keyTok, pm(map[string]any{"name": "ci2", "roles": []string{"admin"}, "description": "first"}))
	d = diff(checked, onlyRenamed)
	assert.Equal(t, p.UpdateReplace, d.DetailedDiff["name"].Kind)
	assert.False(t, d.DeleteBeforeReplace)
	// date-time strings compare as instants
	plusTwo := e.check(keyTok, pm(map[string]any{"name": "ci", "roles": []string{"admin"}, "description": "first", "expiresAt": "2027-01-01T00:00:00+02:00"}))
	zulu := e.check(keyTok, pm(map[string]any{"name": "ci", "roles": []string{"admin"}, "description": "first", "expiresAt": "2026-12-31T22:00:00.000Z"}))
	assert.False(t, diff(plusTwo, zulu).HasChanges)
	later := e.check(keyTok, pm(map[string]any{"name": "ci", "roles": []string{"admin"}, "description": "first", "expiresAt": "2026-12-31T22:00:01Z"}))
	assert.True(t, diff(plusTwo, later).HasChanges)
	// engines that send no old inputs: fall back to the input part of the state
	r, err := e.prov.Diff(ctx, p.DiffRequest{ID: "ci", Urn: urn(keyTok), State: created.Properties, Inputs: roles})
	require.NoError(t, err)
	assert.Equal(t, p.UpdateReplace, r.DetailedDiff["roles"].Kind)

	// update keeps the secret the server will not repeat
	up, err := e.prov.Update(ctx, p.UpdateRequest{ID: "ci", Urn: urn(keyTok), State: created.Properties, OldInputs: checked, Inputs: upd})
	require.NoError(t, err)
	assert.Equal(t, "second", str(t, up.Properties, "description"))
	assert.Equal(t, secret, str(t, up.Properties, "secret"))
	assert.True(t, up.Properties.Get("secret").Secret())
	assert.Contains(t, e.server("GET", "/api/iac/resources/api_keys.api_key?key=ci", ""), `"second"`)

	// update preview: predicted state, no write
	pre, err := e.prov.Update(ctx, p.UpdateRequest{ID: "ci", Urn: urn(keyTok), State: up.Properties, Inputs: checked, DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, "first", str(t, pre.Properties, "description"))
	assert.Equal(t, secret, str(t, pre.Properties, "secret"))
	assert.Contains(t, e.server("GET", "/api/iac/resources/api_keys.api_key?key=ci", ""), `"second"`)

	// the server rejects an immutable change on an existing key: error text is surfaced
	_, err = e.prov.Update(ctx, p.UpdateRequest{ID: "ci", Urn: urn(keyTok), State: up.Properties, Inputs: roles})
	require.Error(t, err)
	assert.ErrorContains(t, err, "HTTP 400")
	assert.ErrorContains(t, err, "immutable")
	assert.ErrorContains(t, err, "issues:")

	// a Delete for a resource created in this very process is the old half of a create-before-delete
	// forced replace (`pulumi up --replace`): the key is shared, so deleting would remove the new object
	require.NoError(t, e.prov.Delete(ctx, p.DeleteRequest{ID: "ci", Urn: urn(keyTok), Properties: created.Properties}))
	assert.Contains(t, e.server("GET", "/api/iac/resources/api_keys.api_key?key=ci", ""), `"second"`, "forced-replace delete must not remove the new object")
	// delete, then read reports it gone
	require.NoError(t, e.prov.Delete(ctx, p.DeleteRequest{ID: "ci", Urn: urn(keyTok), Properties: up.Properties}))
	require.NoError(t, e.prov.Delete(ctx, p.DeleteRequest{ID: "ci", Urn: urn(keyTok)}), "deleting an absent resource succeeds")
	gone, err := e.prov.Read(ctx, p.ReadRequest{ID: "ci", Urn: urn(keyTok), Properties: up.Properties})
	require.NoError(t, err)
	assert.Empty(t, gone.ID)
}

func TestRoleAclLifecycleAndDrift(t *testing.T) {
	e := newEnv(t)
	inputs := pm(map[string]any{"role": "employee", "features": []string{"customers.view"}})

	created := e.create(aclTok, inputs)
	assert.Equal(t, "employee", created.ID)
	assert.Equal(t, false, created.Properties.Get("isSuperAdmin").AsBool())

	// drift behind the provider's back shows up as a changed input on refresh
	e.server("PATCH", "/_fake/auth.role_acl/employee", `{"features":["rogue.feature"],"isSuperAdmin":true}`)
	checked := e.check(aclTok, inputs)
	rd, err := e.prov.Read(ctx, p.ReadRequest{ID: "employee", Urn: urn(aclTok), Properties: created.Properties, Inputs: checked})
	require.NoError(t, err)
	assert.Equal(t, "rogue.feature", rd.Inputs.Get("features").AsArray().Get(0).AsString())
	assert.Equal(t, true, rd.Inputs.Get("isSuperAdmin").AsBool())
	r, err := e.prov.Diff(ctx, p.DiffRequest{ID: "employee", Urn: urn(aclTok), State: rd.Properties, OldInputs: rd.Inputs, Inputs: checked})
	require.NoError(t, err)
	assert.Equal(t, map[string]p.PropertyDiff{
		"features":     {Kind: p.Update, InputDiff: true},
		"isSuperAdmin": {Kind: p.Update, InputDiff: true},
	}, r.DetailedDiff)
	assert.True(t, r.DeleteBeforeReplace, "key unchanged: any replacement must delete first")

	// the update repairs it
	_, err = e.prov.Update(ctx, p.UpdateRequest{ID: "employee", Urn: urn(aclTok), State: rd.Properties, OldInputs: rd.Inputs, Inputs: checked})
	require.NoError(t, err)
	assert.Contains(t, e.server("GET", "/api/iac/resources/auth.role_acl?key=employee", ""), `"customers.view"`)

	// changing the key field is a replace
	other := e.check(aclTok, pm(map[string]any{"role": "admin", "features": []string{"customers.view"}}))
	r, err = e.prov.Diff(ctx, p.DiffRequest{ID: "employee", Urn: urn(aclTok), OldInputs: checked, Inputs: other})
	require.NoError(t, err)
	assert.Equal(t, p.UpdateReplace, r.DetailedDiff["role"].Kind)
	assert.False(t, r.DeleteBeforeReplace, "a new key field value can coexist with the old one")

	// import of an existing role: inputs come back without nulls, so an unchanged program has no diff
	imp, err := e.prov.Read(ctx, p.ReadRequest{ID: "employee", Urn: urn(aclTok)})
	require.NoError(t, err)
	r, err = e.prov.Diff(ctx, p.DiffRequest{ID: "employee", Urn: urn(aclTok), OldInputs: imp.Inputs, Inputs: checked})
	require.NoError(t, err)
	assert.False(t, r.HasChanges)

	// the ACL was created in this process too: that delete is the old half of a forced replace
	require.NoError(t, e.prov.Delete(ctx, p.DeleteRequest{ID: "employee", Urn: urn(aclTok)}))
	assert.NotContains(t, e.server("GET", "/api/iac/resources/auth.role_acl?key=employee", ""), `"features":[]`)
	// delete clears the ACL, the role stays
	require.NoError(t, e.prov.Delete(ctx, p.DeleteRequest{ID: "employee", Urn: urn(aclTok)}))
	assert.Contains(t, e.server("GET", "/api/iac/resources/auth.role_acl?key=employee", ""), `"features":[]`)
}

func TestHTTPErrorsSurface(t *testing.T) {
	e := newEnv(t)
	// unknown role: 400 with {error, issues}
	_, err := e.prov.Create(ctx, p.CreateRequest{Urn: urn(aclTok), Properties: e.check(aclTok, pm(map[string]any{"role": "ghost", "features": []string{}}))})
	require.Error(t, err)
	assert.ErrorContains(t, err, "HTTP 400")
	assert.ErrorContains(t, err, "role does not exist: ghost")
	assert.ErrorContains(t, err, "issues:")

	// bad credentials: 401
	bad := provider.New(version)
	_, err = bad.Parameterize(ctx, p.ParameterizeRequest{Value: &p.ParameterizeRequestValue{Version: version, Value: fake.Kinds}})
	require.NoError(t, err)
	require.NoError(t, bad.Configure(ctx, p.ConfigureRequest{Args: pm(map[string]any{"url": e.srv.URL, "apiKey": "wrong"})}))
	_, err = bad.Read(ctx, p.ReadRequest{ID: "employee", Urn: urn(aclTok)})
	assert.ErrorContains(t, err, "HTTP 401")
}

func TestInvoke(t *testing.T) {
	e := newEnv(t)
	out, err := e.prov.Invoke(ctx, p.InvokeRequest{Token: "talaria:directory:getScope"})
	require.NoError(t, err)
	assert.Equal(t, "Fake Org", str(t, out.Return, "organizationName"))
	assert.Equal(t, "Fake Tenant", str(t, out.Return, "tenantName"))

	_, err = e.prov.Invoke(ctx, p.InvokeRequest{Token: "talaria:auth:RoleAcl"})
	assert.ErrorContains(t, err, "not a data")
	_, err = e.prov.Invoke(ctx, p.InvokeRequest{Token: "talaria:directory:getNothing"})
	assert.ErrorContains(t, err, "unknown token")
}

// A kind whose array items carry defaults, as workflow steps/transitions do: the server (zod)
// fills them in for every item, so Check must too, or a read-back differs from the program.
func TestCheckAppliesDefaultsInsideArrayItems(t *testing.T) {
	raw := []byte(`{"protocol":1,"kinds":[{"name":"mod.thing","mode":"resource","keyField":"name",
		"inputs":{"type":"object","required":["name"],"properties":{"name":{"type":"string"},
			"rules":{"type":"array","items":{"type":"object","required":["id"],"properties":{
				"id":{"type":"string"},"retry":{"type":"boolean","default":false}}}}}},
		"outputs":{"type":"object","properties":{}}}]}`)
	prov := provider.New(version)
	_, err := prov.Parameterize(ctx, p.ParameterizeRequest{Value: &p.ParameterizeRequestValue{Name: "talaria", Version: version, Value: raw}})
	require.NoError(t, err)
	const tok = "talaria:mod:Thing"

	item := func(fields map[string]property.Value) property.Value { return property.New(property.NewMap(fields)) }
	rules := func(items ...property.Value) property.Value { return property.New(property.NewArray(items)) }
	program := property.NewMap(map[string]property.Value{
		"name":  property.New("t"),
		"rules": rules(item(map[string]property.Value{"id": property.New("a")})),
	})
	checked, err := prov.Check(ctx, p.CheckRequest{Urn: urn(tok), Inputs: program})
	require.NoError(t, err)
	require.Empty(t, checked.Failures)
	got := checked.Inputs.Get("rules").AsArray().Get(0).AsMap()
	assert.Equal(t, false, got.Get("retry").AsBool(), "default applied inside the array item")
	assert.Equal(t, "a", got.Get("id").AsString())

	// What a server read-back looks like: the same item with the default filled in. No diff.
	readBack := property.NewMap(map[string]property.Value{
		"name":  property.New("t"),
		"rules": rules(item(map[string]property.Value{"id": property.New("a"), "retry": property.New(false)})),
	})
	d, err := prov.Diff(ctx, p.DiffRequest{ID: "t", Urn: urn(tok), OldInputs: readBack, Inputs: checked.Inputs})
	require.NoError(t, err)
	assert.False(t, d.HasChanges, "defaults in array items must not read as drift: %v", d.DetailedDiff)
}
