import * as pulumi from "@pulumi/pulumi";
import * as talaria from "@pulumi/talaria";

const cfg = new pulumi.Config("e2e");

const acl = new talaria.auth.RoleAcl("employee-acl", {
    role: "employee",
    features: cfg.requireObject<string[]>("features"),
});

const key = new talaria.api_keys.ApiKey("ci-key", {
    name: "ci",
    roles: cfg.requireObject<string[]>("roles"),
    description: cfg.get("description"),
});

const scope = talaria.directory.getScopeOutput();

export const aclFeatures = acl.features;
export const keyId = key.apiKeyId;
export const keyPrefix = key.keyPrefix;
export const secret = key.secret; // must stay secret
export const organizationName = scope.organizationName;
export const organizationId = scope.organizationId;
export const tenantName = scope.tenantName;
