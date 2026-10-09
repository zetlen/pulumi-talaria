# pulumi-talaria

A generic [Pulumi](https://www.pulumi.com) provider for [Talaria](https://github.com/zetlen/talaria)'s
`iac` module. It has no hard-coded resources: it is a *parameterized* provider whose schema is built
from the **kinds document** that Talaria publishes (`GET /api/iac/kinds`, or `yarn mercato iac kinds`),
so new Talaria kinds appear as new Pulumi resources and functions without a provider release.

- Package name: `talaria`; plugin binary: `pulumi-resource-talaria`.
- Resource kind `<module>.<snake_name>` → resource `talaria:<module>:<PascalName>`
  (e.g. `auth.role_acl` → `talaria:auth:RoleAcl`, `talaria.auth.RoleAcl` in TypeScript).
- Data kind → function `talaria:<module>:get<PascalName>` (`directory.scope` → `talaria.directory.getScope`).

## Use

```sh
# Generate a local SDK in a Pulumi project. The argument is a path or http(s) URL to a kinds document;
# a URL is fetched with `Authorization: ApiKey $TALARIA_API_KEY` when that variable is set.
pulumi package add pulumi-resource-talaria ./kinds.json
pulumi package add pulumi-resource-talaria https://talaria.example.com/api/iac/kinds

# or, once the plugin is installed (below):
pulumi package add talaria ./kinds.json
```

The kinds document is embedded in the generated SDK (`package.json` → `pulumi.parameterization.value`), so
later `pulumi up` runs re-parameterize the provider from the SDK; nothing is fetched again.
Only `"protocol": 1` documents are accepted.

```ts
import * as talaria from "@pulumi/talaria";

new talaria.auth.RoleAcl("employee", { role: "employee", features: ["customers.view"] });
const key = new talaria.api_keys.ApiKey("ci", { name: "ci", roles: ["admin"] });
export const secret = key.secret;                 // secret output
const scope = talaria.directory.getScopeOutput(); // data kind
```

Provider configuration (`pulumi config set talaria:url …`, or the environment):

| config   | env               | |
|----------|-------------------|---|
| `url`    | `TALARIA_URL`     | required; Talaria base URL, without `/api/iac` |
| `apiKey` | `TALARIA_API_KEY` | secret; sent as `Authorization: ApiKey <key>` |

## Install on a box

```sh
pulumi plugin install resource talaria <version> --server github://api.github.com/zetlen/pulumi-talaria
```

Tagging `v<version>` runs `.github/workflows/release.yml`, which publishes
`pulumi-resource-talaria-v<version>-{linux-amd64,darwin-arm64,darwin-amd64}.tar.gz` to the GitHub release
(GoReleaser, version baked in with `-ldflags -X main.version`).

## The protocol (version 1)

HTTP JSON under `{url}/api/iac`, header `Authorization: ApiKey <secret>`. Tenant/organization come from
the key. Errors are non-2xx with `{ "error": string, "issues"?: ZodIssue[] }`; the provider reports both.

| | |
|---|---|
| `GET /kinds` | kinds document: `{protocol, kinds: [{name, description, mode, keyField, inputs, outputs, secretFields, replaceOnChanges}]}`; `inputs`/`outputs` are JSON Schema (zod 4) objects |
| `GET /resources/{kind}?key=k1&key=k2` | `{items: {k1: State, k2: null}}`; `null` = does not exist |
| `PUT /resources/{kind}/{key}` | body = desired inputs → State; idempotent create-or-update; body's `keyField` must equal `{key}` |
| `DELETE /resources/{kind}/{key}` | 204, also when already absent |
| `POST /data/{kind}` | body = inputs → outputs (data kinds) |

State = inputs ∪ outputs as one flat object. A kind's `get` must return every input; it may omit an output
it cannot read back (e.g. a secret only shown at creation), in which case the provider keeps the last known value.

### How kinds map to Pulumi

- **Schema**: `inputs` → input properties; properties = inputs ∪ outputs; `required` honored (nullable
  `anyOf [X, {type: null}]` → optional `X`); `secretFields` → `secret: true`; `default` kept for primitives and
  applied by `Check` for everything. Supported JSON Schema: string/number/integer/boolean, string `enum`
  (plain string), arrays, nullable, `additionalProperties` maps, nested objects (named types under `types`).
  Anything else fails schema generation with the kind and field named.
- **ID** is the `keyField` value. Changing it, or any `replaceOnChanges` input, replaces the resource;
  since the replacement has the same key, it is *delete before create*.
- **Diff** compares new inputs with the previous inputs field by field (`null` ≡ absent; secret flag ignored).
- **Refresh/import** (`Read`) return the server state; the inputs they return contain only input fields. On refresh,
  optional inputs the program never set stay out of the inputs, so server-side defaults (e.g. an API key's
  `organizationId`) do not show up as drift. A freshly *imported* resource has every server value as input —
  mirror them in the program.
- **`id` / `urn` fields**: Pulumi reserves these on resources, so a kind field `id` is exposed as
  `<camelName>Id` (`api_keys.api_key.id` → `apiKeyId`). Data kinds are unaffected.

## Develop

```sh
go vet ./... && go test ./...   # unit tests against an in-memory fake of the protocol (internal/fake)
./scripts/e2e.sh                # builds the plugin + fake server and drives real Pulumi (TypeScript, local backend)
```

`scripts/e2e.sh` is the acceptance test: `package add`, `up`, no-op `preview`, in-place update, replace, drift via
`refresh`, `import`, `destroy`. It needs `pulumi`, `node`/`npm`, `jq`, `curl` and network access for `npm install`.
