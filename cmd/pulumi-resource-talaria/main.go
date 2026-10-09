// Command pulumi-resource-talaria is the Pulumi provider plugin for Talaria's iac module.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/blang/semver"
	p "github.com/pulumi/pulumi-go-provider"

	"github.com/zetlen/pulumi-talaria/provider"
)

// version is set at release time: -ldflags "-X main.version=1.2.3".
var version = "0.0.0-dev"

func main() {
	v, err := semver.Parse(version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid version %q: %s\n", version, err)
		os.Exit(1)
	}
	if err := p.RunProvider(context.Background(), provider.BaseName, version, provider.New(v)); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}
