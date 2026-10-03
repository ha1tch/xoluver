// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

package xoluver

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// unit: Version, the VERSION file and the README status line must agree. A
// release that bumps one and forgets another fails here.
func TestVersionMatchesVersionFile(t *testing.T) {
	b, err := os.ReadFile("VERSION")
	if err != nil {
		t.Fatalf("reading VERSION: %v", err)
	}
	if got := strings.TrimSpace(string(b)); got != Version {
		t.Fatalf("VERSION file says %q, const Version says %q (run `repoman syncver check`)", got, Version)
	}
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(Version) {
		t.Fatalf("Version %q is not MAJOR.MINOR.PATCH", Version)
	}
	r, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}
	if !strings.Contains(string(r), "Version "+Version+";") {
		t.Fatalf("README.md status line does not carry Version %s", Version)
	}
}
