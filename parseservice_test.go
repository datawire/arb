package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseService(t *testing.T) {
	tests := []struct {
		input    string
		expected ArbService
		err      string
	}{
		{
			input: "http://localhost:8080/",
			expected: ArbService{
				URL:   "http://localhost:8080/",
				Codes: nil,
			},
			err: "",
		},
		{
			input: "http://localhost:8080/ (200)",
			expected: ArbService{
				URL:   "http://localhost:8080/",
				Codes: []int{200},
			},
			err: "",
		},
		{
			input: "http://localhost:8080/ (200, 503)",
			expected: ArbService{
				URL:   "http://localhost:8080/",
				Codes: []int{200, 503},
			},
			err: "",
		},
		{
			input: "http://localhost:8080/ (200,503,429)",
			expected: ArbService{
				URL:   "http://localhost:8080/",
				Codes: []int{200, 503, 429},
			},
			err: "",
		},
		{
			input: "http://localhost:8080/ (200,503,429)",
			expected: ArbService{
				URL:   "http://localhost:8080/",
				Codes: []int{200, 503, 429},
			},
			err: "",
		},

		{
			input:    "",
			expected: ArbService{},
			err:      "invalid service: ''",
		},
		{
			input:    "http://localhost/ 5000",
			expected: ArbService{},
			err:      "invalid service: 'http://localhost/ 5000'",
		},
		{
			input:    ":foo (5000)",
			expected: ArbService{},
			err:      "invalid URL ':foo' in ':foo (5000)': parse \":foo\": missing protocol scheme",
		},
		{
			input:    "http://localhost/ (5000aa)",
			expected: ArbService{},
			err:      "invalid service: 'http://localhost/ (5000aa)'",
		},
		{
			input:    "http://localhost/ (200 503 429)",
			expected: ArbService{},
			err:      "invalid code '200 503 429' in service 'http://localhost/ (200 503 429)'",
		},
		{
			input:    "http://localhost/ (200, aa, 429)",
			expected: ArbService{},
			err:      "invalid service: 'http://localhost/ (200, aa, 429)'",
		},
	}

	for _, test := range tests {
		actual, err := parseService(test.input)

		if test.err == "" {
			require.NoError(t, err)
		} else {
			require.Equal(t, test.err, err.Error())
		}

		require.Equal(t, test.expected, actual)
	}
}
