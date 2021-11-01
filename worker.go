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

	// Send all entries

	allEntries, _ := json.Marshal(rawEntries)
	dlog.Infof(ctx, "==== %d: %s ", w.id, w.service)
	dlog.Infof(ctx, "%s", string(allEntries))
}
