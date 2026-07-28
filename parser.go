package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
)

// rawRequest holds the parsed pieces of a pasted request, independent of
// which tool's format it was pasted from.
type rawRequest struct {
	method string
	url    string
	header http.Header
	body   string
}

// requestParser converts a pasted request in one specific tool format into
// a rawRequest.
//
// To support a new source — e.g. Chrome devtools' "Copy as cURL (bash)" —
// write a parse function in its own file (parser_proxyman.go shows the
// shape) and add one entry to requestParsers below. Nothing else changes.
type requestParser struct {
	name  string
	parse func(raw string) (*rawRequest, error)
}

// requestParsers is the registry of known formats. Order only matters for
// request_format: "auto", which tries them top to bottom and keeps the
// first one that succeeds.
var requestParsers = []requestParser{
	{"proxyman-raw", parseProxymanRaw},
	{"chrome-curl", parseChromeCurl},
}

// loadRequest reads the pasted request from path and parses it with the
// configured format ("auto" tries every registered parser in order). It
// returns the parsed request plus the name of the parser that produced it.
func loadRequest(path, format string) (*rawRequest, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("reading %s: %w", path, err)
	}
	raw := string(data)

	if format != "auto" {
		for _, p := range requestParsers {
			if p.name == format {
				req, err := p.parse(raw)
				return req, p.name, err
			}
		}
		return nil, "", fmt.Errorf("unknown request_format %q (registered: %s)", format, parserNames())
	}

	var errs []string
	for _, p := range requestParsers {
		req, err := p.parse(raw)
		if err == nil {
			return req, p.name, nil
		}
		errs = append(errs, p.name+": "+err.Error())
	}
	return nil, "", fmt.Errorf("no registered parser accepted the file:\n  %s", strings.Join(errs, "\n  "))
}

func parserNames() string {
	names := make([]string, 0, len(requestParsers))
	for _, p := range requestParsers {
		names = append(names, p.name)
	}
	return strings.Join(names, ", ")
}
