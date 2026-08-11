package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	kitdaemon "go.kenn.io/kit/daemon"
	kwt "go.kenn.io/kwt"
	kwtdaemon "go.kenn.io/kwt/internal/daemon"
	"go.kenn.io/kwt/service"
)

type statusProvider struct {
	status kwtdaemon.Status
}

type failingInventory struct{}

func (failingInventory) Query(context.Context, kwt.Request) (kwt.Result, error) {
	return kwt.Result{}, service.NewError(
		service.InventoryFailed, "inventory source failed", false, nil, nil,
	)
}

func (failingInventory) ApproveConfig(context.Context, kwt.ConfigApproval) error {
	return nil
}

func (p statusProvider) Status(time.Time) kwtdaemon.Status { return p.status }

func main() {
	home := flag.String("home", "", "kwt home")
	mode := flag.String("mode", "", "fixture mode")
	ready := flag.String("ready", "", "readiness marker")
	flag.Parse()
	if *home == "" || *ready == "" {
		log.Fatal("home and ready are required")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	endpoint, err := kitdaemon.ParseEndpoint(
		listener.Addr().String(),
		kitdaemon.ParseEndpointOptions{TCPPolicy: kitdaemon.RequireLoopback},
	)
	if err != nil {
		log.Fatal(err)
	}
	build := kwtdaemon.Build{Version: "v1.0.0", Revision: strings.Repeat("a", 40)}
	record, token, err := kwtdaemon.NewRuntimeRecord(*home, build, endpoint)
	if err != nil {
		log.Fatal(err)
	}
	schemaMajor := kwtdaemon.APISchemaMajor
	schemaVersion := kwtdaemon.APISchemaVersion
	if *mode == "incompatible" {
		schemaMajor = 2
		schemaVersion = "2.0.0"
		record.Metadata["schema_major"] = strconv.Itoa(schemaMajor)
		record.Metadata["schema_version"] = schemaVersion
	}
	if _, err := kwtdaemon.RuntimeStore(*home).Write(record); err != nil {
		log.Fatal(err)
	}
	if *mode == "unresponsive" {
		_ = listener.Close()
		if err := os.WriteFile(*ready, []byte("ready"), 0o600); err != nil {
			log.Fatal(err)
		}
		select {}
	}

	proof, err := kitdaemon.NewProof([]byte(token))
	if err != nil {
		log.Fatal(err)
	}
	ping, err := proof.NewPingHandler(record)
	if err != nil {
		log.Fatal(err)
	}
	capabilities := strings.Split(record.Metadata["capabilities"], ",")
	started := time.Now()
	status := kwtdaemon.Status{
		Service: kwtdaemon.ServiceName, State: kwtdaemon.StateReady,
		Home: *home, Endpoint: listener.Addr().String(), PID: os.Getpid(),
		Version: build.Version, Revision: build.Revision,
		SchemaMajor: schemaMajor, SchemaVersion: schemaVersion,
		Capabilities: capabilities, StartedAt: started,
	}
	if *mode == "draining" {
		status.State = kwtdaemon.StateDraining
		deadline := time.Now().Add(-10 * time.Second)
		status.DrainDeadline = &deadline
	}
	provider := statusProvider{status: status}
	serverOptions := kwtdaemon.ServerOptions{
		Token: token, ExpectedHost: listener.Addr().String(), Status: provider,
		Shutdown: func(context.Context, kwtdaemon.ShutdownRequest) (kwtdaemon.Status, error) {
			return provider.status, nil
		},
		Ping: ping,
	}
	if *mode == "inventory_failed" {
		serverOptions.Inventory = failingInventory{}
	}
	handler := kwtdaemon.NewServer(serverOptions)
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	if err := os.WriteFile(*ready, []byte("ready"), 0o600); err != nil {
		log.Fatal(err)
	}
	select {}
}
