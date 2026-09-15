// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"flag"
	"log"
	"runtime/debug"

	"github.com/hashicorp/terraform-plugin-go/tfprotov5"
	"github.com/hashicorp/terraform-plugin-go/tfprotov5/tf5server"
	"github.com/hashicorp/terraform-provider-aws/internal/idempotency"
	"github.com/hashicorp/terraform-provider-aws/internal/provider"
	"github.com/hashicorp/terraform-provider-aws/version"
)

func main() {
	debugFlag := flag.Bool("debug", false, "Start provider in debug mode.")
	flag.Parse()

	logFlags := log.Flags()
	logFlags = logFlags &^ (log.Ldate | log.Ltime)
	log.SetFlags(logFlags)

	if buildInfo, ok := debug.ReadBuildInfo(); ok {
		log.Printf("Starting %s@%s (%s)...", buildInfo.Main.Path, version.ProviderVersion, buildInfo.GoVersion)
	}

	serverFactory, _, err := provider.ProtoV5ProviderServerFactory(context.Background())

	if err != nil {
		log.Fatal(err)
	}

	// Wrap the muxed provider server so every ApplyResourceChange call is
	// bracketed by a CallScope: on a normal return, AWS calls the resource
	// handler made are promoted from "responded" to "completed" in the
	// idempotency journal; on an interrupted/cancelled call, they are
	// released back to "pending" so a re-run can safely reclaim them. See
	// internal/idempotency/server.go's doc comment for why this step is
	// required, not optional, for the journal's correctness -- and
	// docs/DEV_PLAN.md for the overall design.
	//
	// idempotency.Enabled() (TF_AWS_IDEMPOTENCY_DISABLE) lets this be
	// turned off entirely without a rebuild, e.g. to compare behavior
	// against an unmodified provider.
	if idempotency.Enabled() {
		store, err := idempotency.GlobalStore(context.Background())
		if err != nil {
			log.Printf("[WARN] idempotency: could not open journal, continuing without deterministic tokens: %s", err)
		} else {
			inner := serverFactory
			serverFactory = func() tfprotov5.ProviderServer {
				return idempotency.WrapProviderServer(inner(), store)
			}
			defer func() {
				if err := idempotency.CloseGlobalStore(); err != nil {
					log.Printf("[WARN] idempotency: closing journal: %s", err)
				}
			}()
		}
	}

	var serveOpts []tf5server.ServeOpt

	if *debugFlag {
		serveOpts = append(serveOpts, tf5server.WithManagedDebug())
	}

	err = tf5server.Serve(
		"registry.terraform.io/hashicorp/aws",
		serverFactory,
		serveOpts...,
	)

	if err != nil {
		log.Fatal(err)
	}
}
