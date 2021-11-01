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

type Worker struct {
	mutex          sync.Mutex
	config         *ArbConfig
	id             int
	service        string               // cached copy of the service URL
	entryChannel   chan json.RawMessage // Channel to receive JSON entries
	pendingEntries []json.RawMessage    // List of JSON entries to be processed
}

func NewWorker(config *ArbConfig, id int) *Worker {
	return &Worker{
		config:         config,
		id:             id,
		service:        config.services[id],
		entryChannel:   make(chan json.RawMessage),
		pendingEntries: make([]json.RawMessage, 0),
	}
}

// Add writes a new entry onto our Worker's entryChannel for its Manager goroutine
// to pick up.
func (w *Worker) Add(ctx context.Context, entry json.RawMessage) {
	w.entryChannel <- entry
}

// Enqueue grabs the mutex and actually writes the given entry into our queue.
func (w *Worker) Enqueue(ctx context.Context, entry json.RawMessage) {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	w.pendingEntries = append(w.pendingEntries, entry)
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

// Run should be called as a goroutine and managed by dgroup.
func (w *Worker) Run(ctx context.Context) error {
	dlog.Infof(ctx, "Worker starting for %s", w.service)

	for {
		threshold := w.config.batchSize

		select {
		case entry := <-w.entryChannel:
			w.Enqueue(ctx, entry)
			dlog.Infof(ctx, "Worker %d: new entry", w.id)

		case <-time.After(w.config.batchDelay):
			dlog.Infof(ctx, "Worker %d: delay expired", w.id)
			threshold = 0

		case <-ctx.Done():
			dlog.Infof(ctx, "Worker %d: shutting down", w.id)
			return nil
		}

		dlog.Infof(ctx, "Worker %d: %d entries pending, threshold %d", w.id, len(w.pendingEntries), threshold)

		if len(w.pendingEntries) > threshold {
			w.sendall(ctx)
		}
	}
}

// sendall handles the heavy lifting of actually sending requests
// to the service.
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

		// dlog.Infof(ctx, "Wrk %d: sending %d entries, attempt %d", w.id, numEntries, attempt)

		status := w.attempt(ctx, allEntries)

		if status == http.StatusOK {
			// All good; we're done here.
			dlog.Infof(ctx, "Wrk %d: %s OK!", w.id, w.service)
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

			dlog.Errorf(ctx, "FAILED: %s got %s on final retry", w.service, errstr)
			return
		}

		// We're allowed to retry. Wait for the retry delay and try again.
		dlog.Infof(ctx, "Wrk %d: %s will retry %d in %s", w.id, w.service, status, retryDelay)
		retryDelay = retryDelay * time.Duration(w.config.retryMultiplier)

		time.Sleep(retryDelay)
	}
}

// attempt makes a single attempt to send the entries to the upstream service.
func (w *Worker) attempt(ctx context.Context, allEntries []byte) int {
	// dlog.Infof(ctx, "Wrk %d (%s): %s", w.id, w.service, string(allEntries))

	// This is a pretty straightforward HTTP POST; we just need to be sure to set
	// the Content-Type header to application/json.
	req, err := http.NewRequestWithContext(ctx, "POST", w.service, bytes.NewBuffer(allEntries))

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
