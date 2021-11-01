// Copyright (c) 2021 Ambassador Labs, Inc. See LICENSE for license information.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/datawire/dlib/dgroup"
	"github.com/datawire/dlib/dhttp"
	"github.com/datawire/dlib/dlog"
	als_service_v2 "github.com/envoyproxy/go-control-plane/envoy/service/accesslog/v2"
	"github.com/golang/protobuf/jsonpb"
	"google.golang.org/grpc"
)

// server is a glue type that lets us glue our logic into the ALS handler in the
// go-control-plane. The "marshaler" is solely for the glue, but the "workers" array
// stores important state for our implementation.
type server struct {
	marshaler jsonpb.Marshaler // Marshaler for entries
	workers   []*Worker        // One worker per endpoint
}

// Compile-time assertion to verify that the server type actually implements the
// interface we need it to.
var _ als_service_v2.AccessLogServiceServer = &server{}

// NewALS creates a new instance of our server, including creating a worker for each
// upstream service.
func NewALS(config *ArbConfig) *server {
	// Not exactly magic here. Just allocate the workers array, then populate it.
	workers := make([]*Worker, len(config.services))

	for i, _ := range config.services {
		workers[i] = NewWorker(config, i)
	}

	// We don't need to do anything with the marshaler here.
	return &server{
		workers: workers,
	}
}

// StreamAccessLogs is called for every new gRPC stream that connects to the ALS. The
// stream is a sequence of als_service_v2.StreamAccessLogsMessage, which in turn contain
// one or more envoy_data_accesslog_v2.HTTPAccessLogEntry, which we need to hand to all of
// our workers.
func (s *server) StreamAccessLogs(stream als_service_v2.AccessLogService_StreamAccessLogsServer) error {
	dlog.Infof(stream.Context(), "Started stream")

	for {
		// Grab the ALS message. If something goes wrong, we return nil to shut the stream
		// down.
		in, err := stream.Recv()

		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		// So far so good. Iterate over all the log entries from the message.
		for _, entry := range in.GetHttpLogs().LogEntry {
			// We marshal the entry into JSON only once -- no sense running that
			// code for each worker.
			entryJSON, _ := s.marshaler.MarshalToString(entry)

			// Once marshaled, hand it off to each worker.
			for _, worker := range s.workers {
				// dlog.Debugf(stream.Context(), "Sending to worker %d", worker.id)
				worker.Add(stream.Context(), json.RawMessage(entryJSON))
			}
		}
	}
}

func main() {
	// Start by setting up our context.
	ctx := context.Background()

	// After that, read our configuration.
	config, err := readArbConfig(ctx, "/etc/arb-config")

	if err != nil {
		dlog.Errorf(ctx, "Failed to read config from /etc/arb-config: %s", err)
		os.Exit(1)
	}

	dlog.Infof(ctx, "ARB startup: batchSize       %v", config.batchSize)
	dlog.Infof(ctx, "ARB startup: batchDelay      %v", config.batchDelay)
	dlog.Infof(ctx, "ARB startup: requestTimeout  %v", config.requestTimeout)
	dlog.Infof(ctx, "ARB startup: retries         %v", config.retries)
	dlog.Infof(ctx, "ARB startup: retryDelay      %v", config.retryDelay)
	dlog.Infof(ctx, "ARB startup: retryMultiplier %v", config.retryMultiplier)
	dlog.Infof(ctx, "ARB startup: services        %v", config.services)

	als := NewALS(config)

	// We're going to be running tasks in parallel, so we'll use dgroup to manage
	// a group of goroutines to do so.
	grp := dgroup.NewGroup(ctx, dgroup.GroupConfig{
		// Enable signal handling so that SIGINT can start a graceful shutdown,
		// and a second SIGINT will force a not-so-graceful shutdown. This shutdown
		// will be signaled to the worker goroutines through the Context that gets
		// passed to them.
		EnableSignalHandling: true,
	})

	grp.Go("gRPC", func(ctx context.Context) error {
		grpcHandler := grpc.NewServer()
		als_service_v2.RegisterAccessLogServiceServer(grpcHandler, als)

		sc := &dhttp.ServerConfig{
			Handler: grpcHandler,
		}

		dlog.Info(ctx, "starting...")
		return sc.ListenAndServe(ctx, ":9001")
	})

	for _, worker := range als.workers {
		grp.Go(fmt.Sprintf("worker%d", worker.id), worker.Run)
	}

	err = grp.Wait()

	if err != nil {
		dlog.Errorf(ctx, "finished with error: %v", err)
		os.Exit(1)
	}
}
