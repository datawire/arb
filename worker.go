// Copyright (c) 2021 Ambassador Labs, Inc. See LICENSE for license information.

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/datawire/dlib/dlog"
)

// A Worker is a thing that manages communications with a single upstream
// service. There are two goroutines active for each Worker:
//
// - The Manager goroutine listens for new entries from the gRPC server, queues
//   them up, and triggers the Worker when it's time to send a new batch of entries.
//
// - The Worker goroutine responds to triggers from the Manager goroutine, manages
//   communications, and handles retries, etc.

type Worker struct {
	config  *ArbConfig // The configuration for this whole ARB
	id      int        // The index of service we're managing
	service ArbService // Cached copy of the service
	pretty  string     // Cached pretty-printed version of the service

	mutex          sync.Mutex        // Protects access to pendingEntries
	pendingEntries []json.RawMessage // List of JSON entries to be processed

	entryChannel   chan json.RawMessage // Channel to receive JSON entries from gRPC
	triggerChannel chan struct{}        // Channel for the manager to trigger the worker
}

// NewWorker creates a new Worker. It does _not_ start any goroutines.
func NewWorker(config *ArbConfig, id int) *Worker {
	return &Worker{
		config:         config,
		id:             id,
		service:        config.services[id],
		pretty:         config.services[id].String(),
		pendingEntries: make([]json.RawMessage, 0),
		entryChannel:   make(chan json.RawMessage),
		triggerChannel: make(chan struct{}),
	}
}

// String returns a human-readable representation of this worker.
func (w *Worker) String() string {
	return w.pretty
}

// Accepts returns true IFF this worker will accept the given status code.
func (w *Worker) Accepts(statusCode int) bool {
	// If no codes are specified, accept everything.
	if len(w.service.Codes) == 0 {
		return true
	}

	// OK, we have some codes specified, so check to see if we have a match.
	//
	// XXX Yes, linear searches are grim, but there really shouldn't be more
	// than a few in our list, so whatever.
	for _, code := range w.service.Codes {
		if code == statusCode {
			return true
		}
	}

	// We had some codes and none of them matched, so we won't accept this.
	return false
}

// Add writes a new entry onto our Worker's entryChannel for its Manager goroutine
// to pick up.
func (w *Worker) Add(ctx context.Context, entry json.RawMessage) {
	w.entryChannel <- entry
}

// Enqueue grabs the mutex and actually writes the given entry into our queue.
// It returns the number of messages dropped from the queue due to it being full.
func (w *Worker) Enqueue(ctx context.Context, entry json.RawMessage) int {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	w.pendingEntries = append(w.pendingEntries, entry)

	// Assume that we don't need to drop any entries...
	dropped := 0

	// ...then actually check, and drop if need be.
	//
	// XXX This is ripe for optimization... later.
	if (w.config.queueSize > 0) && (len(w.pendingEntries) > w.config.queueSize) {
		dropped = len(w.pendingEntries) - w.config.queueSize
		w.pendingEntries = w.pendingEntries[dropped:]
		// dlog.Debugf(ctx, "Mgr %d: queue size exceeded, dropped %d %s", w.id, dropped, PluralEntry(delta))
	}

	return dropped
}

// GrabEntries grabs the mutex and pulls all the pendingEntries off the queue,
// returning the entries and emptying the pendingEntries queue so that the
// Manager can start adding new entries as they arrive.
func (w *Worker) GrabEntries(ctx context.Context) []json.RawMessage {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	allEntries := w.pendingEntries
	w.pendingEntries = make([]json.RawMessage, 0)

	return allEntries
}

// Trigger triggers the worker goroutine to actually send the next batch of
// entries.
func (w *Worker) Trigger(ctx context.Context) {
	// This is just a nonblocking send on our triggerChannel.
	select {
	case w.triggerChannel <- struct{}{}:
		break
	default:
		break
	}
}

// RunManager should be called as a goroutine and managed by dgroup.
//
// It listens for new entries from the gRPC server, queues them up, and triggers the
// Worker when it's time to send a new batch of entries.
func (w *Worker) RunManager(ctx context.Context) error {
	dlog.Infof(ctx, "Mgr %d: starting for %s", w.id, w)

	// Keep track of when we last triggered the worker.
	lastTriggered := time.Now()

	// Keep track of the last time we logged about dropped entries.
	lastLoggedDrops := time.Now()
	dropped := 0

	// Loop forever looking for things to do.
	for {
		// Start by waiting for something to happen.
		select {
		case entry := <-w.entryChannel:
			// A new entry has arrived, so we need to add it to our queue.
			justDropped := w.Enqueue(ctx, entry)
			dropped += justDropped
			dlog.Debugf(ctx, "Mgr %d: new entry (dropped %d)", w.id, justDropped)

		case <-time.After(w.config.batchDelay):
			// Make sure that we wake up at least once every batchDelay, in case
			// traffic is really bursty: if we get a partial batch, then there's a
			// long delay before the next message, we don't want to stall until the
			// next message arrives.
			dlog.Debugf(ctx, "Mgr %d: delay expired", w.id)

		case <-ctx.Done():
			// Shutdown! We're done here.
			dlog.Infof(ctx, "Mgr %d: shutting down", w.id)
			return nil
		}

		// If here, something has happened. If we have at least a full batch of
		// messages, or we have at least one message and it's been at least
		// one batch delay since the last trigger, then it's time to trigger the
		// worker.

		numEntries := len(w.pendingEntries)
		dlog.Debugf(ctx, "Mgr %d: %d %s pending, batchSize %d", w.id, numEntries, PluralEntry(numEntries), w.config.batchSize)

		trigger := false

		if numEntries >= w.config.batchSize {
			dlog.Debugf(ctx, "Mgr %d: triggering due to full batch", w.id)
			trigger = true
		} else if (numEntries > 0) && (time.Since(lastTriggered) >= w.config.batchDelay) {
			dlog.Debugf(ctx, "Mgr %d: triggering due to batch delay", w.id)
			trigger = true
		}

		if trigger {
			// Trigger the worker...
			w.Trigger(ctx)

			// ...and reset the lastTriggered time so we don't instantly trigger it
			// next time through!
			lastTriggered = time.Now()
		}

		// If we've dropped any messages, and it's been at least five minutes since
		// we last logged about it, log about it again.
		if (dropped > 0) && (time.Since(lastLoggedDrops) > (5 * time.Minute)) {
			dlog.Warnf(ctx, "Mgr %d: dropped %d %s in the last five minutes", w.id, dropped, PluralEntry(dropped))
			lastLoggedDrops = time.Now()
			dropped = 0
		}
	}
}

// RunWorker should be called as a goroutine and managed by dgroup.
// Its purpose is to actually send groups of entries to the upstream
// service, managing retries and backoff.
func (w *Worker) RunWorker(ctx context.Context) error {
	dlog.Infof(ctx, "Wrk %d: starting for %s", w.id, w)

	// Loop forever looking for things to do.
	for {
		select {
		case <-w.triggerChannel:
			// We've been triggered to send a batch of entries. Hit it!
			// dlog.Debugf(ctx, "Wrk %d: triggered", w.id)
			w.sendall(ctx)

		case <-ctx.Done():
			// Shutdown! We're done here.
			dlog.Infof(ctx, "Wrk %d: shutting down", w.id)
			return nil
		}
	}
}

// sendall handles the heavy lifting of actually sending requests to the upstream
// service.
func (w *Worker) sendall(ctx context.Context) {
	// Grab our pendingEntries.
	rawEntries := w.GrabEntries(ctx)

	// If there are somehow no entries, we're done. This should be impossible.
	numEntries := len(rawEntries)

	if numEntries == 0 {
		return
	}

	// OK, we have some entries. Marshal them up as JSON (this can't actually
	// fail, since the entries are already json.RawMessages)...
	allEntries, _ := json.Marshal(rawEntries)

	// ...and dive into the retry loop.
	attempt := 0
	retryDelay := w.config.retryDelay

	for {
		// Increment attempt here so that the log message looks better.
		attempt++

		// dlog.Debugf(ctx, "Wrk %d: sending %d %s, attempt %d", w.id, numEntries, PluralEntry(numEntries), attempt)

		status := w.attempt(ctx, allEntries)

		if status == http.StatusOK {
			// All good; we're done here.
			dlog.Debugf(ctx, "Wrk %d: %s OK!", w.id, w)
			return
		}

		// Something has gone wrong. Can we retry?
		canRetry := IsRetryable(status)

		if !canRetry || (attempt >= w.config.retries) {
			// Bzzzt.
			errstr := "client-side failure"

			if status >= 0 {
				errstr = fmt.Sprintf("%d", status)
			}

			dlog.Errorf(ctx, "FAILED: %s got %s on final retry", w, errstr)
			return
		}

		// We're allowed to retry. Wait for the retry delay and try again.
		dlog.Debugf(ctx, "Wrk %d: %s will retry %d in %s", w.id, w, status, retryDelay)
		retryDelay = retryDelay * time.Duration(w.config.retryMultiplier)

		time.Sleep(retryDelay)
	}
}

// attempt makes a single attempt to send the entries to the upstream service.
func (w *Worker) attempt(ctx context.Context, allEntries []byte) int {
	// dlog.Infof(ctx, "Wrk %d (%s): %s", w.id, w.service, string(allEntries))

	// This is a pretty straightforward HTTP POST; we just need to be sure to set
	// the Content-Type header to application/json.
	req, err := http.NewRequestWithContext(ctx, "POST", w.service.URL, bytes.NewBuffer(allEntries))

	if err != nil {
		dlog.Errorf(ctx, "Wrk %d: failed to create request: %s", w.id, err)
		return -1
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: w.config.requestTimeout}

	if os.Getenv("ARB_INSECURE_TLS") == "true" {
		// No, no indeed, it is _not_ a great idea to disable server cert verifications.
		// But the user is asking for it, so...
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

		client.Transport = tr
	}

	resp, err := client.Do(req)

	if err != nil {
		dlog.Errorf(ctx, "Wrk %d: failed to send request: %s", w.id, err)
		return -1
	}

	// It always feels weird to me to defer a close so late, but, well, can't defer
	// it until we've checked the error from client.Do, so oh well.
	defer resp.Body.Close()

	return resp.StatusCode
}
