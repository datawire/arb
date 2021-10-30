// Copyright (c) 2021 Ambassador Labs, Inc. See LICENSE for license information.

package main

import "net/http"

//////////////// Utility functions

// Pluralize returns the singular form of a word if n == 1, or the plural form otherwise.
func Pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// PluralEntry uses Pluralize to return the correct word for the number of entries.
func PluralEntry(n int) string {
	return Pluralize(n, "entry", "entries")
}

// IsRetryable returns true if the given HTTP status is something we should retry.
// A special case here is that a status less than 0 is used for a client-side failure,
// like we couldn't create an http.Request or the like.
func IsRetryable(status int) bool {
	switch {
	case status < 0:
		// This was a failure in our client code -- we never got far enough to try
		// talking to the server. It's already been logged and it's probably OK to
		// retry. Most likely anyway.
		return true

	case status < 500:
		// "Wait wait," I hear you cry, "don't you mean to be retrying the 4YZ codes??"
		//
		// Well... no. 4YZ is "the client screwed something up" -- it's unlikely to
		// work next time if the client screwed the request up! The 5YZ series are
		// the server having problems, and they are much more likely to work next
		// time... except for the handful that aren't, which we call out explicitly
		// here.
		//
		// For those of us who've been at this awhile, this feels very weird, but
		// really, the world has changed since the early 2000s.
		return false

	// Some 5YZ codes are not retryable... or at least they're probably not
	// likely to work if retried.
	case status == http.StatusNotImplemented:
		return false
	case status == http.StatusHTTPVersionNotSupported:
		return false
	case status == http.StatusNetworkAuthenticationRequired:
		return false

	case status >= 600:
		// This is "impossible".
		return false
	}

	// If we get here, we're looking at a 5YZ code that is probably OK to retry.
	//
	// See the "status < 500" clause, above, for more on why the 5YZs are the
	// ones to retry.
	return true
}
