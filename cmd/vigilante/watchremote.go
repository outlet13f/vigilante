package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// remotePoll is how often `watch --server` asks for the verdict.
var remotePoll = 2 * time.Second

// gateCodes are the server's codes for a closed deployment gate.
var gateCodes = map[string]bool{"circuit_open": true, "change_frozen": true, "change_ticket_invalid": true, "itsm_unavailable": true}

// apiError is a non-2xx answer from the server. Code is the v1 "code" (or
// v2 problem "code") when the body has one.
type apiError struct {
	Method, Path string
	Status       int
	Code         string
	Body         string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s %s: %d %s", e.Method, e.Path, e.Status, e.Body)
}

// apiRequest is apiCall with extra headers and a typed error.
func apiRequest(ctx context.Context, server, method, path string, hdr http.Header, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(server, "/")+path, rd)
	if err != nil {
		return err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := os.Getenv("VIGILANTE_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		e := &apiError{Method: method, Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
		var b struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(raw, &b) == nil {
			e.Code = b.Code
		}
		return e
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// remoteExitCode maps a failed create to the exit code local mode uses: a
// closed gate is 3, anything else 1. Servers that predate the v1 "code"
// answer 409 on that endpoint only for a closed gate.
func remoteExitCode(err error) int {
	var ae *apiError
	if !errors.As(err, &ae) {
		return 1
	}
	if gateCodes[ae.Code] || (ae.Code == "" && ae.Status == http.StatusConflict) {
		return 3
	}
	return 1
}
