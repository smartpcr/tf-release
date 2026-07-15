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

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.local/smartpcr/labdeploy",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
