package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/provider"
)

// set by goreleaser: -ldflags "-X main.version=..."
var version = "0.1.0"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run provider with debugger support")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), opts(debug))
	if err != nil {
		log.Fatal(err)
	}
}

// opts builds the provider serve options from the package-level ServeOpts source
// of truth (registry.local/smartpcr/labdeploy) and layers the runtime debug flag
// on top, so the served address stays in sync with the acceptance harness.
func opts(debug bool) providerserver.ServeOpts {
	o := provider.ServeOpts()
	o.Debug = debug
	return o
}
