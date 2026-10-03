// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

// Package demo holds the small plain-HTTP helpers the examples share. xoluver
// versions entities; creating and reading the live entity is ordinary xolu API
// use, which is all this package does.
package demo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

// BaseURL is XOLU_URL, or http://localhost:9090.
func BaseURL() string {
	if u := os.Getenv("XOLU_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://localhost:9090"
}

// Must stops the program on an error.
func Must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// Call sends a JSON request, decodes a JSON response into out (if not nil) and
// returns the status. Numbers decode as json.Number so none lose digits.
// Any HTTP error status stops the program.
func Call(method, path string, in, out any) int {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		Must(err)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, BaseURL()+path, body)
	Must(err)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	Must(err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	Must(err)
	if resp.StatusCode >= 400 {
		log.Fatalf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, data)
	}
	if out != nil {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		Must(dec.Decode(out))
	}
	return resp.StatusCode
}

// CreateAsset creates an entity of type "asset" and returns its id. A new
// entity is at version 1.
func CreateAsset(doc map[string]any) int {
	var out struct {
		ID int `json:"id"`
	}
	Call(http.MethodPost, "/api/v1/asset", doc, &out)
	return out.ID
}

// Load reads the live entity: its document and its current version.
func Load(id int) (doc map[string]any, version int) {
	Call(http.MethodGet, fmt.Sprintf("/api/v1/asset/%d", id), nil, &doc)
	n, _ := doc["_version"].(json.Number)
	v, _ := n.Int64()
	return doc, int(v)
}
