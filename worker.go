// Copyright (c) 2021 Ambassador Labs, Inc. See LICENSE for license information.

package main

import (
	"context"
	"encoding/json"
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

// Add adds a new entry to our worker's queue.
func (w *Worker) Add(ctx context.Context, entry json.RawMessage) {
	w.entryChannel <- entry
}

// Run should be called as a goroutine and managed by dgroup.
func (w *Worker) Run(ctx context.Context) error {
	dlog.Infof(ctx, "Worker starting for %s", w.service)

	for {
		threshold := w.config.batchSize

		select {
		case entry := <-w.entryChannel:
			// This is a CLOSURE, not just a random anonymous function.
			// The point is that we want to defer the unlock, but we
			// also don't want to be holding the lock while logging.
			func() {
				w.mutex.Lock()
				defer w.mutex.Unlock()
				w.pendingEntries = append(w.pendingEntries, entry)
			}()

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
	w.mutex.Lock()
	defer w.mutex.Unlock()

	if len(w.pendingEntries) == 0 {
		return
	}

	// Send all entries

	allEntries, _ := json.Marshal(w.pendingEntries)
	dlog.Infof(ctx, "==== %d: %s: sending %s", w.id, w.service, string(allEntries))

	w.pendingEntries = make([]json.RawMessage, 0)
}
