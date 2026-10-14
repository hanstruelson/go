// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types

import (
	"fmt"
	"internal/goversion"
	"internal/lazyregexp"
	"strconv"
)

// LangVersion is a language version broken into major and minor numbers.
type LangVersion struct {
	major, minor int
}

// AllowsGoVersion reports whether local package is allowed
// to use Go version major.minor.
func (v LangVersion) AllowsGoVersion(major, minor int) bool {
	if v.major == 0 && v.minor == 0 {
		return true
	}
	return v.major > major || (v.major == major && v.minor >= minor)
}

// ParseLangFlag validates a -lang value and returns its package-local version.
func ParseLangFlag(s string) (LangVersion, error) {
	if s == "" {
		return LangVersion{}, nil
	}

	langWant, err := parseLang(s)
	if err != nil {
		return LangVersion{}, fmt.Errorf("invalid value %q for -lang: %w", s, err)
	}

	if def := currentLang(); s != def {
		defVers, err := parseLang(def)
		if err != nil {
			return LangVersion{}, fmt.Errorf("internal error parsing default lang %q: %w", def, err)
		}
		if langWant.major > defVers.major || (langWant.major == defVers.major && langWant.minor > defVers.minor) {
			return LangVersion{}, fmt.Errorf("invalid value %q for -lang: max known version is %q", s, def)
		}
	}
	return langWant, nil
}

// parseLang parses a -lang option into a langVer.
func parseLang(s string) (LangVersion, error) {
	if s == "go1" { // cmd/go's new spelling of "go1.0" (#65528)
		s = "go1.0"
	}

	matches := goVersionRE.FindStringSubmatch(s)
	if matches == nil {
		return LangVersion{}, fmt.Errorf(`should be something like "go1.12"`)
	}
	major, err := strconv.Atoi(matches[1])
	if err != nil {
		return LangVersion{}, err
	}
	minor, err := strconv.Atoi(matches[2])
	if err != nil {
		return LangVersion{}, err
	}
	return LangVersion{major: major, minor: minor}, nil
}

// currentLang returns the current language version.
func currentLang() string {
	return fmt.Sprintf("go1.%d", goversion.Version)
}

// goVersionRE is a regular expression that matches the valid
// arguments to the -lang flag.
var goVersionRE = lazyregexp.New(`^go([1-9]\d*)\.(0|[1-9]\d*)$`)
