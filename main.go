// Copyright (c) 2021 Ambassador Labs, Inc. See LICENSE for license information.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/datawire/dlib/dgroup"
	"github.com/datawire/dlib/dhttp"
	"github.com/datawire/dlib/dlog"
	als_service_v2 "github.com/envoyproxy/go-control-plane/envoy/service/accesslog/v2"
	"github.com/golang/protobuf/jsonpb"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

//////////////// AccessLogService glue

// server is a glue type that lets us glue our logic into the ALS handler in the
// go-control-plane. The "marshaler" is solely for the glue, but the "workers" array
// stores important state for our implementation.
type server struct {
	marshaler       jsonpb.Marshaler // Marshaler for entries
	workers         []*Worker        // One worker per endpoint
	watchdogChannel chan struct{}    // Channel to reset the watchdog
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
		workers:         workers,
		watchdogChannel: make(chan struct{}, 0),
	}
}

// StreamAccessLogs is called for every new gRPC stream that connects to the ALS. The
// stream is a sequence of als_service_v2.StreamAccessLogsMessage, which in turn contain
// one or more envoy_data_accesslog_v2.HTTPAccessLogEntry, which we need to hand to all of
// our workers.
func (s *server) StreamAccessLogs(stream als_service_v2.AccessLogService_StreamAccessLogsServer) error {
	dlog.Debugf(stream.Context(), "Envoy connected")

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

		// Grab the individual log entries from the message...
		entries := in.GetHttpLogs().LogEntry
		numEntries := len(entries)
		dlog.Debugf(stream.Context(), "gRPC: received %d %s", numEntries, PluralEntry(numEntries))

		// ...reset the watchdog, since we've gotten some traffic...
		s.watchdogChannel <- struct{}{}

		// ...then iterate over all the log entries from the message and feed them to
		// our upstream workers.
		for _, entry := range entries {
			// We marshal the entry into JSON only once -- no sense running that
			// code for each worker.
			entryJSON, _ := s.marshaler.MarshalToString(entry)

			// After marshaling the entry, we turn it into a json.RawMessage, which
			// makes it easier for the workers to wrangle when they hand multiple
			// entries upstream.
			rawEntry := json.RawMessage(entryJSON)
			// dlog.Debugf(stream.Context(), "gRPC: received entry %d", rawEntry)

			// OK. What status code is this entry?
			statusCode := entry.Response.ResponseCode.Value
			// dlog.Debugf(stream.Context(), "gRPC: status code %d", statusCode)

			// Finally, hand rawEntry off to each worker that'll accept this status
			// code.
			for _, worker := range s.workers {
				if worker.Accepts(int(statusCode)) {
					// dlog.Debugf(stream.Context(), "gRPC: sending to worker %d", worker.id)
					worker.Add(stream.Context(), rawEntry)
				}
			}
		}
	}
}

//////////////// Traffic Watchdog

// Watchdog should be started as a gorutine and managed with dgroup. It keeps track
// of how long it's been since an event last arrived on the watchdog channel: if it's
// been more than an hour, it warns the user to check their LogService configuration.
func (s *server) Watchdog(ctx context.Context) error {
	dlog.Infof(ctx, "WATCHDOG: starting")

	// Loop forever, checking the watchdog channel.
	for {
		select {
		case <-s.watchdogChannel:
			// OK, some traffic has arrived. Awesome.
			dlog.Debugf(ctx, "WATCHDOG: reset")

		case <-time.After(time.Hour):
			// It's been an hour without receiving any traffic on the
			// watchdogChannel. Warn the user that something may be wrong.
			dlog.Warnf(ctx, "WARNING: no requests in one hour; check your LogService configuration")

		case <-ctx.Done():
			// Shutdown! We're done here.
			dlog.Infof(ctx, "WATCHDOG: shutting down")
			return nil
		}
	}
}

//////////////// Logging/context glue

func ContextWithLogrusLogging(ctx context.Context, cmdname string, loglevel string) context.Context {
	// Grab a Logrus logger and default it to INFO...
	logrusLogger := logrus.New()
	logrusLogger.SetLevel(logrus.InfoLevel)

	// ...then set up dlog and a context with it.
	logger := dlog.WrapLogrus(logrusLogger).
		WithField("PID", os.Getpid()).
		WithField("CMD", cmdname)

	newContext := dlog.WithLogger(ctx, logger)

	// Once _that's_ done, we can check to see if our caller wants a different
	// log level.
	//
	// XXX Why not do this before creating the logger?? Well, we kinda need a
	// context and dlog so that we can log the error if the loglevel is bad...
	if loglevel != "" {
		parsed, err := logrus.ParseLevel(loglevel)

		if err != nil {
			dlog.Errorf(newContext, "Error parsing log level %s: %v", loglevel, err)
		} else {
			logrusLogger.SetLevel(parsed)
		}
	}

	return newContext
}

//////////////// Mainline

func main() {
	// Start by setting up logging, which is intimately tied into setting up
	// our context.
	ctx := ContextWithLogrusLogging(context.Background(), "ARB", os.Getenv("ARB_LOG_LEVEL"))

	// After that, read our configuration.
	config, err := readArbConfig(ctx, "/etc/arb-config")

	if err != nil {
		dlog.Errorf(ctx, "Failed to read config from /etc/arb-config: %s", err)
		os.Exit(1)
	}

	dlog.Infof(ctx, "ARB startup: queueSize       %v", config.queueSize)
	dlog.Infof(ctx, "ARB startup: batchSize       %v", config.batchSize)
	dlog.Infof(ctx, "ARB startup: batchDelay      %v", config.batchDelay)
	dlog.Infof(ctx, "ARB startup: requestTimeout  %v", config.requestTimeout)
	dlog.Infof(ctx, "ARB startup: retries         %v", config.retries)
	dlog.Infof(ctx, "ARB startup: retryDelay      %v", config.retryDelay)
	dlog.Infof(ctx, "ARB startup: retryMultiplier %v", config.retryMultiplier)

	serviceStrings := make([]string, 0, len(config.services))

	for _, service := range config.services {
		serviceStrings = append(serviceStrings, service.String())
	}

	dlog.Infof(ctx, "ARB startup: services        %v", strings.Join(serviceStrings, ", "))

	// Grab a new server instance using the config we just read.
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

	// Start the watchdog goroutine first.
	grp.Go("watchdog", als.Watchdog)

	// Fire up two goroutines for each upstream. The Manager manages the queue of
	// incoming events; the Worker manages actually talking to those upstreams.
	for _, worker := range als.workers {
		grp.Go(fmt.Sprintf("mgr%d", worker.id), worker.RunManager)
		grp.Go(fmt.Sprintf("wrk%d", worker.id), worker.RunWorker)
	}

	// Fire up the gRPC server. Don't think too hard about the fact that we're
	// using an HTTP server for gRPC: gRPC _is_ HTTP/2, after all...
	grp.Go("gRPC", func(ctx context.Context) error {
		grpcHandler := grpc.NewServer()
		als_service_v2.RegisterAccessLogServiceServer(grpcHandler, als)

		sc := &dhttp.ServerConfig{
			Handler: grpcHandler,
		}

		dlog.Infof(ctx, "Listening on port %d...", config.port)
		return sc.ListenAndServe(ctx, fmt.Sprintf(":%d", config.port))
	})

	// After that, just wait to shutdown.
	err = grp.Wait()

	if err != nil {
		dlog.Errorf(ctx, "finished with error: %v", err)
		os.Exit(1)
	}
}
