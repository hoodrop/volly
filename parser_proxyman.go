package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// parseProxymanRaw parses a raw HTTP request block as copied from Proxyman's
// RAW view (the same format browser devtools' "Copy as raw" produces):
//
//	POST /path HTTP/1.1
//	Host: example.com
//	Header-Name: value
//	...
//	<blank line>
//	{...body...}
//
// "https" is prepended to the Host header to build the full URL (raw requests
// don't carry the scheme). Content-Length is stripped since Go's http.Client
// sets it automatically based on the body, and a stale value copied from
// devtools would break the request.
func parseProxymanRaw(raw string) (*rawRequest, error) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	reader := bufio.NewReader(strings.NewReader(raw))

	// Request line: "POST /path HTTP/1.1"
	requestLine, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("reading request line: %w", err)
	}
	requestLine = strings.TrimSpace(requestLine)
	parts := strings.Fields(requestLine)
	if len(parts) < 2 {
		return nil, fmt.Errorf("malformed request line: %q", requestLine)
	}
	method := parts[0]
	path := parts[1]

	header := make(http.Header)
	host := ""

	for {
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			break // EOF with nothing left, no body present
		}
		trimmed := strings.TrimRight(line, "\n")
		trimmed = strings.TrimRight(trimmed, "\r")

		if trimmed == "" {
			break // blank line = end of headers, body follows
		}

		colonIdx := strings.Index(trimmed, ":")
		if colonIdx == -1 {
			continue // skip malformed header lines
		}
		name := strings.TrimSpace(trimmed[:colonIdx])
		value := strings.TrimSpace(trimmed[colonIdx+1:])

		// Skip Content-Length: Go sets this automatically from the body,
		// and a stale/incorrect value copied from devtools would break the request.
		if strings.EqualFold(name, "Content-Length") {
			continue
		}
		if strings.EqualFold(name, "Host") {
			host = value
			continue // Host is set separately via req.Host / URL, not as a normal header
		}

		header.Add(name, value)

		if err != nil {
			break
		}
	}

	// Whatever remains is the body.
	bodyBytes, _ := io.ReadAll(reader)
	body := strings.TrimRight(string(bodyBytes), "\n")
	body = strings.TrimRight(body, "\r")

	if host == "" {
		return nil, fmt.Errorf("no Host header found in raw request")
	}
	fullURL := "https://" + host + path

	return &rawRequest{
		method: method,
		url:    fullURL,
		header: header,
		body:   body,
	}, nil
}
