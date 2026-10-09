package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/blang/semver"
	"github.com/pulumi/pulumi/sdk/v3/go/property"

	p "github.com/pulumi/pulumi-go-provider"
)

// New returns the provider. It serves the bare base provider until Parameterize is called.
func New(version semver.Version) p.Provider {
	s := &state{version: version}
	return p.Provider{
		Parameterize: s.parameterize,
		GetSchema:    s.getSchema,
		Configure:    s.configure,
		Check:        s.check,
		Diff:         s.diff,
		Create:       s.create,
		Read:         s.read,
		Update:       s.update,
		Delete:       s.delete,
		Invoke:       s.invoke,
	}
}

type state struct {
	version semver.Version
	raw     []byte
	doc     *KindsDoc
	byToken map[string]*Kind
	client  *client
	written sync.Map // kind name + "\x00" + key of objects this process PUT and has not deleted since
}

// parameterize captures the kinds document, from a path/URL (CLI) or the embedded
// parameter bytes (re-parameterization from a generated SDK).
func (s *state) parameterize(ctx context.Context, req p.ParameterizeRequest) (p.ParameterizeResponse, error) {
	var raw []byte
	switch {
	case req.Args != nil:
		if len(req.Args.Args) != 1 {
			return p.ParameterizeResponse{}, fmt.Errorf("expected exactly one argument (path or http(s) URL of a kinds document), got %d", len(req.Args.Args))
		}
		var err error
		if raw, err = loadKinds(ctx, req.Args.Args[0]); err != nil {
			return p.ParameterizeResponse{}, err
		}
	case req.Value != nil:
		raw = req.Value.Value
	default:
		return p.ParameterizeResponse{}, fmt.Errorf("missing parameterization arguments")
	}
	doc, err := ParseKinds(raw)
	if err != nil {
		return p.ParameterizeResponse{}, err
	}
	s.raw, s.doc, s.byToken = raw, doc, map[string]*Kind{}
	for _, k := range doc.Kinds {
		s.byToken[k.Token()] = k
	}
	return p.ParameterizeResponse{Name: PackageName, Version: s.version}, nil
}

func loadKinds(ctx context.Context, src string) ([]byte, error) {
	if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") {
		return os.ReadFile(src)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	if key := os.Getenv("TALARIA_API_KEY"); key != "" {
		req.Header.Set("Authorization", "ApiKey "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, httpError("GET", src, resp.StatusCode, data)
	}
	return data, nil
}

func (s *state) getSchema(context.Context, p.GetSchemaRequest) (p.GetSchemaResponse, error) {
	spec, err := BuildSchema(s.doc, s.raw, s.version.String())
	if err != nil {
		return p.GetSchemaResponse{}, err
	}
	b, err := json.Marshal(spec)
	return p.GetSchemaResponse{Schema: string(b)}, err
}

func (s *state) configure(_ context.Context, req p.ConfigureRequest) error {
	get := func(name, env string) string {
		if v, ok := req.Args.GetOk(name); ok && v.IsString() && v.AsString() != "" {
			return v.AsString()
		}
		return os.Getenv(env)
	}
	url := get("url", "TALARIA_URL")
	if url == "" {
		return fmt.Errorf("talaria: url is required (provider config talaria:url or env TALARIA_URL)")
	}
	s.client = newClient(url, get("apiKey", "TALARIA_API_KEY"))
	return nil
}

// kind resolves the kind of the given token and mode.
func (s *state) kind(token, mode string) (*Kind, error) {
	k := s.byToken[token]
	if k == nil {
		return nil, fmt.Errorf("unknown token %q: not in the parameterized kinds document", token)
	}
	if k.Mode != mode {
		return nil, fmt.Errorf("%s is a %s kind, not a %s", token, k.Mode, mode)
	}
	return k, nil
}

func (s *state) check(_ context.Context, req p.CheckRequest) (p.CheckResponse, error) {
	k, err := s.kind(req.Urn.Type().String(), modeResource)
	if err != nil {
		return p.CheckResponse{}, err
	}
	in := applyDefaults(k.Inputs, renameKeys(req.Inputs, k.protocolName))
	var failures []p.CheckFailure
	for _, name := range required(k.Inputs) {
		if v, ok := in.GetOk(name); !ok || v.IsNull() {
			failures = append(failures, p.CheckFailure{Property: k.exposed(name), Reason: "missing required property"})
		}
	}
	if v, ok := in.GetOk(k.KeyField); ok && !v.IsNull() && !v.IsComputed() && !v.IsString() {
		failures = append(failures, p.CheckFailure{Property: k.exposed(k.KeyField), Reason: "key field must be a string"})
	}
	return p.CheckResponse{Inputs: renameKeys(in, k.exposed), Failures: failures}, nil
}

// differs reports whether two input values differ. Null and absent are the same; secrets
// are compared by value; an unknown new value always differs. date-time strings (the field's
// schema is `string`/`date-time`) are equal when they are the same instant, since the server
// may answer in another offset or precision.
func differs(schema map[string]any, old, new property.Value) (bool, error) {
	if new.HasComputed() {
		return true, nil
	}
	a, err := toJSON(old)
	if err != nil {
		return false, err
	}
	b, err := toJSON(new)
	if err != nil {
		return false, err
	}
	if inner, ok := nullable(schema); ok {
		schema = inner
	}
	if schema["type"] == "string" && schema["format"] == "date-time" {
		as, aok := a.(string)
		bs, bok := b.(string)
		if aok && bok {
			ta, errA := time.Parse(time.RFC3339Nano, as)
			tb, errB := time.Parse(time.RFC3339Nano, bs)
			if errA == nil && errB == nil {
				return !ta.Equal(tb), nil
			}
		}
	}
	return !reflect.DeepEqual(a, b), nil
}

func (s *state) diff(_ context.Context, req p.DiffRequest) (p.DiffResponse, error) {
	k, err := s.kind(req.Urn.Type().String(), modeResource)
	if err != nil {
		return p.DiffResponse{}, err
	}
	old, inputs := renameKeys(req.OldInputs, k.protocolName), renameKeys(req.Inputs, k.protocolName)
	if old.Len() == 0 { // engines that do not send old inputs: fall back to the input part of state
		old = pick(renameKeys(req.State, k.protocolName), properties(k.Inputs))
	}
	resp := p.DiffResponse{DetailedDiff: map[string]p.PropertyDiff{}}
	keyChanged := false
	schemas := properties(k.Inputs)
	names := map[string]bool{}
	for n := range old.All {
		names[n] = true
	}
	for n := range inputs.All {
		names[n] = true
	}
	for _, name := range sortedKeys(names) {
		o, n := old.Get(name), inputs.Get(name)
		changed, err := differs(schemas[name], o, n)
		if err != nil {
			return resp, fmt.Errorf("%s: %w", name, err)
		}
		if !changed {
			continue
		}
		kind := p.Update
		switch {
		case o.IsNull():
			kind = p.Add
		case n.IsNull():
			kind = p.Delete
		}
		if k.isReplace(name) {
			kind += "&replace"
		}
		if name == k.KeyField {
			keyChanged = true
		}
		resp.DetailedDiff[k.exposed(name)] = p.PropertyDiff{Kind: kind, InputDiff: true}
	}
	// The kind's object is identified by its key. While the key is unchanged, a replacement would
	// collide with the old object, so it must go first. A changed key lets both coexist.
	resp.DeleteBeforeReplace = !keyChanged
	resp.HasChanges = len(resp.DetailedDiff) > 0
	return resp, nil
}

func pick(m property.Map, keep map[string]map[string]any) property.Map {
	out := map[string]property.Value{}
	for k, v := range m.All {
		if keep[k] != nil {
			out[k] = v
		}
	}
	return property.NewMap(out)
}

// putState writes inputs and returns the resulting state: sent inputs, then previous outputs,
// then whatever the server returned (the server may omit outputs it cannot read back).
func (s *state) putState(ctx context.Context, k *Kind, inputs, prev property.Map) (property.Map, error) {
	keyV, ok := inputs.GetOk(k.KeyField)
	if !ok || !keyV.IsString() {
		return property.Map{}, fmt.Errorf("%s: key field %q must be a string", k.Name, k.KeyField)
	}
	body, err := mapToJSON(inputs, properties(k.Inputs))
	if err != nil {
		return property.Map{}, err
	}
	resp, err := s.client.put(ctx, k.Name, keyV.AsString(), body)
	if err != nil {
		return property.Map{}, err
	}
	s.written.Store(k.Name+"\x00"+keyV.AsString(), struct{}{})
	st := pick(inputs, properties(k.Inputs))
	for n, v := range pick(prev, properties(k.Outputs)).All {
		st = st.Set(n, v)
	}
	return k.fromServer(st, resp), nil
}

// fromServer overlays a server state onto st (declared fields only) and marks secrets.
func (k *Kind) fromServer(st property.Map, server map[string]any) property.Map {
	in, out := properties(k.Inputs), properties(k.Outputs)
	for n, x := range server {
		if in[n] != nil || out[n] != nil {
			st = st.Set(n, fromJSON(x))
		}
	}
	return k.markSecrets(st)
}

func (k *Kind) markSecrets(m property.Map) property.Map {
	for _, f := range k.SecretFields {
		if v, ok := m.GetOk(f); ok {
			m = m.Set(f, v.WithSecret(true))
		}
	}
	return m
}

// preview predicts state during a dry run: inputs, previous outputs, unknown for the rest.
func (k *Kind) preview(inputs, prev property.Map) property.Map {
	st := pick(inputs, properties(k.Inputs))
	for n := range properties(k.Outputs) {
		if v, ok := prev.GetOk(n); ok {
			st = st.Set(n, v)
		} else {
			st = st.Set(n, property.New(property.Computed))
		}
	}
	return k.markSecrets(st)
}

func (s *state) create(ctx context.Context, req p.CreateRequest) (p.CreateResponse, error) {
	k, err := s.kind(req.Urn.Type().String(), modeResource)
	if err != nil {
		return p.CreateResponse{}, err
	}
	inputs := renameKeys(req.Properties, k.protocolName)
	if req.DryRun {
		return p.CreateResponse{Properties: renameKeys(k.preview(inputs, property.Map{}), k.exposed)}, nil
	}
	st, err := s.putState(ctx, k, inputs, property.Map{})
	if err != nil {
		return p.CreateResponse{}, err
	}
	id := st.Get(k.KeyField).AsString()
	return p.CreateResponse{ID: id, Properties: renameKeys(st, k.exposed)}, nil
}

func (s *state) update(ctx context.Context, req p.UpdateRequest) (p.UpdateResponse, error) {
	k, err := s.kind(req.Urn.Type().String(), modeResource)
	if err != nil {
		return p.UpdateResponse{}, err
	}
	inputs, prev := renameKeys(req.Inputs, k.protocolName), renameKeys(req.State, k.protocolName)
	if req.DryRun {
		return p.UpdateResponse{Properties: renameKeys(k.preview(inputs, prev), k.exposed)}, nil
	}
	st, err := s.putState(ctx, k, inputs, prev)
	return p.UpdateResponse{Properties: renameKeys(st, k.exposed)}, err
}

func (s *state) delete(ctx context.Context, req p.DeleteRequest) error {
	k, err := s.kind(req.Urn.Type().String(), modeResource)
	if err != nil {
		return err
	}
	// The engine ignores Diff's DeleteBeforeReplace for a forced replace (`--replace`) and for a
	// rename without an alias: it creates the new resource first, under the same key, then deletes
	// the old one. Deleting by key now would remove the object this run just wrote, so a delete of a
	// key this process PUT (and has not deleted since) is a no-op.
	if _, ok := s.written.LoadAndDelete(k.Name + "\x00" + req.ID); ok {
		return nil
	}
	return s.client.delete(ctx, k.Name, req.ID)
}

// read refreshes (or imports) a resource. Input fields come only from the server so drift
// shows; output fields the server omits keep their last known value.
func (s *state) read(ctx context.Context, req p.ReadRequest) (p.ReadResponse, error) {
	k, err := s.kind(req.Urn.Type().String(), modeResource)
	if err != nil {
		return p.ReadResponse{}, err
	}
	server, err := s.client.get(ctx, k.Name, req.ID)
	if err != nil {
		return p.ReadResponse{}, err
	}
	if server == nil {
		return p.ReadResponse{}, nil
	}
	st := pick(renameKeys(req.Properties, k.protocolName), properties(k.Outputs))
	st = k.fromServer(st, server)
	inputs := map[string]property.Value{}
	for n := range properties(k.Inputs) {
		if x, ok := server[n]; ok && x != nil {
			inputs[n] = fromJSON(x)
		}
	}
	if req.Inputs.Len() > 0 { // refresh, not import
		// Optional fields the program left unset may be filled in by the server (e.g. organizationId
		// defaults to the key's organization); reporting them as inputs would show up as drift,
		// or even a replacement, on the next preview.
		prev := renameKeys(req.Inputs, k.protocolName)
		for n := range inputs {
			if v, ok := prev.GetOk(n); !ok || v.IsNull() {
				delete(inputs, n)
			}
		}
	}
	return p.ReadResponse{
		ID: req.ID, Properties: renameKeys(st, k.exposed),
		Inputs: renameKeys(k.markSecrets(property.NewMap(inputs)), k.exposed),
	}, nil
}

func (s *state) invoke(ctx context.Context, req p.InvokeRequest) (p.InvokeResponse, error) {
	k, err := s.kind(req.Token.String(), modeData)
	if err != nil {
		return p.InvokeResponse{}, err
	}
	args, err := mapToJSON(req.Args, properties(k.Inputs))
	if err != nil {
		return p.InvokeResponse{}, err
	}
	out, err := s.client.data(ctx, k.Name, args)
	if err != nil {
		return p.InvokeResponse{}, err
	}
	return p.InvokeResponse{Return: k.fromServer(property.Map{}, out)}, nil
}
