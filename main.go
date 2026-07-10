// terraform-provider-fcs is the Terraform provider for tenant-scoped FCS
// platform resources.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/provider"
)

// Set via -ldflags at release time, e.g.
//
//	go build -ldflags "-X main.version=0.1.0"
var version = "dev"

// providerAddress is the registry address; keep it in sync with the module
// path in go.mod.
const providerAddress = "registry.terraform.io/focusnetcloud/fcs"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run the provider with support for debuggers (delve)")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: providerAddress,
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
