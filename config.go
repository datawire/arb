// Copyright (c) 2021 Ambassador Labs, Inc. See LICENSE for license information.

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/datawire/dlib/dlog"
	"github.com/pkg/errors"
)

type ArbConfig struct {
	port            int           // port we'll listen on
	queueSize       int           // maximum number of messages to buffer
	batchSize       int           // number of requests to batch together
	batchDelay      time.Duration // delay between batches
	requestTimeout  time.Duration // timeout for each request
	retries         int           // number of times to retry a request
	retryDelay      time.Duration // initial delay between retries
	retryMultiplier int           // multiplier for each subsequent retry
	services        []string      // upstream services to send to
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

// readArbConfig: read the ARB config from the given directory
func readArbConfig(ctx context.Context, dirPath string) (*ArbConfig, error) {
	config := &ArbConfig{}

	config.port = readIntWithDefault(ctx, dirPath, "port", 9001)
	config.queueSize = readIntWithDefault(ctx, dirPath, "queueSize", 4096)
	config.batchSize = readIntWithDefault(ctx, dirPath, "batchSize", 5)
	config.batchDelay = readDurationWithDefault(ctx, dirPath, "batchDelay", 30*time.Second)
	config.requestTimeout = readDurationWithDefault(ctx, dirPath, "requestTimeout", 250*time.Millisecond)
	config.retries = readIntWithDefault(ctx, dirPath, "retries", 3)
	config.retryDelay = readDurationWithDefault(ctx, dirPath, "retryDelay", 30*time.Second)
	config.retryMultiplier = readIntWithDefault(ctx, dirPath, "retryMultiplier", 2)

	services, err := stringsFromFile(dirPath + "/services")

	if err != nil {
		return nil, errors.Wrap(err, fmt.Sprintf("failed to read services from %s", dirPath))
	}

	config.services = services

	return config, nil
}
