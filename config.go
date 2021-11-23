// Copyright (c) 2021 Ambassador Labs, Inc. See LICENSE for license information.

package main

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/datawire/dlib/dlog"
	"github.com/pkg/errors"
)

type ArbService struct {
	URL   string // URL to contact
	Codes []int  // HTTP status codes to send to this URL
}

func (svc *ArbService) String() string {
	statusStrings := make([]string, 0)

	for _, status := range svc.Codes {
		statusStrings = append(statusStrings, strconv.Itoa(status))
	}

	statuses := strings.Join(statusStrings, ",")

	if len(statuses) == 0 {
		statuses = "*"
	}

	return fmt.Sprintf("%s (%s)", svc.URL, statuses)
}

type ArbConfig struct {
	port            int           // port we'll listen on
	queueSize       int           // maximum number of messages to buffer
	batchSize       int           // number of requests to batch together
	batchDelay      time.Duration // delay between batches
	requestTimeout  time.Duration // timeout for each request
	retries         int           // number of times to retry a request
	retryDelay      time.Duration // initial delay between retries
	retryMultiplier int           // multiplier for each subsequent retry
	services        []ArbService  // upstream services to send to
}

// stringsFromFile: read all strings from a file
func stringsFromFile(path string) ([]string, error) {
	file, err := os.Open(path)

	if err != nil {
		return nil, errors.Wrap(err, fmt.Sprintf("failed to open %s", path))
	}

	defer file.Close()

	strings := make([]string, 0)

	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		strings = append(strings, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return nil, errors.Wrap(err, fmt.Sprintf("failed to read %s", path))
	}

	return strings, nil
}

// stringFromFile: read a single string from a file
func stringFromFile(path string, defaultValue string) (string, error) {
	strings, err := stringsFromFile(path)

	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return defaultValue, nil
		}

		return defaultValue, err
	}

	if len(strings) == 0 {
		return defaultValue, errors.New(fmt.Sprintf("no strings found in %s", path))
	}

	return strings[0], nil
}

// intFromFile: read a single int from a file
func intFromFile(path string, defaultValue int) (int, error) {
	// Pass the default value as a string to stringFromFile, so that we get
	// a good value in case the file doesn't exist.
	s, err := stringFromFile(path, fmt.Sprintf("%d", defaultValue))

	if err != nil {
		// We don't check for fs.ErrExist here because stringFromFile will
		// have already caught that.
		return defaultValue, err
	}

	i, err := strconv.Atoi(s)

	if err != nil {
		return defaultValue, errors.Wrap(err, fmt.Sprintf("read invalid int from %s: '%s'", path, s))
	}

	return i, nil
}

// readIntWithDefault: read an int from a file. If something goes wrong, log an error
// and use a given default value.
func readIntWithDefault(ctx context.Context, root string, name string, defaultValue int) int {
	i, err := intFromFile(root+"/"+name, defaultValue)

	if err != nil {
		dlog.Errorf(ctx, "failed to read %s from %s, defaulting to %d: %s", name, root, defaultValue, err)
		return defaultValue
	}

	return i
}

// readDurationWithDefault: read a duration from a file. If something goes wrong, log an
// error and use a given default value.
func readDurationWithDefault(ctx context.Context, root string, name string, defaultValue time.Duration) time.Duration {
	s, err := stringFromFile(root+"/"+name, defaultValue.String())

	if err != nil {
		dlog.Errorf(ctx, "failed to read %s from %s, defaulting to %s: %s", name, root, defaultValue, err)
		return defaultValue
	}

	d, err := time.ParseDuration(s)

	if err != nil {
		dlog.Errorf(ctx, "failed to parse %s from %s, defaulting to %s: %s", name, root, defaultValue, err)
		return defaultValue
	}

	return d
}

// parseService parses a single service line, which looks like:
//
// URL (code1, code2, ...)
//
// The codes are optional; if present, they must be numeric HTTP status codes, and only
// entries with a matching status code will be sent to the URL. If no codes are present,
// all requests will be sent to the URL.
func parseService(rawService string) (ArbService, error) {
	// I hope Golang knows how to only compile these regexes once. [ :) ]
	serviceRE := regexp.MustCompile(`^([^ ]+)( \(([0-9 ,]+)\))?$`)
	commaRE := regexp.MustCompile(`, ?`)

	// OK: serviceRE describes our line as a whole, with groups for the relevant
	// parts. Does the line match it?
	//
	// (Obviously we could've written a little recursive-descent parser or a DFA
	// or the like, but meh, no point for this syntax.)
	matches := serviceRE.FindStringSubmatch(rawService)

	if matches == nil {
		// It doesn't match at all. Bzzzzt.
		return ArbService{}, fmt.Errorf("invalid service: '%s'", rawService)
	}

	// OK, it matches, so we know we have a URL (or, more accurately, we have a thing
	// that could be a URL. We'll use the URL package to parse it to make sure it's OK.
	_, err := url.Parse(matches[1])

	if err != nil {
		return ArbService{}, errors.Wrap(err, fmt.Sprintf("invalid URL '%s' in '%s'", matches[1], rawService))
	}

	// OK, the URL passes muster, so stash it in a new ArbService...
	service := ArbService{
		URL: matches[1],
	}

	// ...and check to see if we have any codes.
	if len(matches[3]) > 0 {
		// Yup, so we need to check them out, too. Make an empty array to keep them in...
		service.Codes = make([]int, 0)

		// ...and then use commaRE to split the string into individual codes that we can
		// look at.
		for _, code := range commaRE.Split(matches[3], -1) {
			// A non-numeric code is an error (it's probably due to a missing comma, but still.)
			codeInt, err := strconv.Atoi(code)

			if err != nil {
				return ArbService{}, fmt.Errorf("invalid code '%s' in service '%s'", code, rawService)
			}

			// All good -- stash this code and continue.
			service.Codes = append(service.Codes, codeInt)
		}
	}

	// If here, we have a valid service. Onward.
	return service, nil
}

// readArbConfig: read the ARB config from the given directory
func readArbConfig(ctx context.Context, dirPath string) (*ArbConfig, error) {
	config := &ArbConfig{}

	// Most of the entries in the configuration are straightforward: read a single
	// line, parse it, and stash it in the appropriate field.
	config.port = readIntWithDefault(ctx, dirPath, "port", 9001)
	config.queueSize = readIntWithDefault(ctx, dirPath, "queueSize", 4096)
	config.batchSize = readIntWithDefault(ctx, dirPath, "batchSize", 5)
	config.batchDelay = readDurationWithDefault(ctx, dirPath, "batchDelay", 30*time.Second)
	config.requestTimeout = readDurationWithDefault(ctx, dirPath, "requestTimeout", 250*time.Millisecond)
	config.retries = readIntWithDefault(ctx, dirPath, "retries", 3)
	config.retryDelay = readDurationWithDefault(ctx, dirPath, "retryDelay", 30*time.Second)
	config.retryMultiplier = readIntWithDefault(ctx, dirPath, "retryMultiplier", 2)

	// The services are a little trickier. We'll read them in one at a time and use
	// parseService to parse them.
	rawServices, err := stringsFromFile(dirPath + "/services")

	if err != nil {
		return nil, errors.Wrap(err, fmt.Sprintf("failed to read services from %s", dirPath))
	}

	config.services = make([]ArbService, 0)

	for _, rawService := range rawServices {
		service, err := parseService(rawService)

		if err != nil {
			return nil, errors.Wrap(err, fmt.Sprintf("failed to parse service %s", rawService))
		}

		config.services = append(config.services, service)
	}

	return config, nil
}
