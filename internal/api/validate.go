package api

import (
	"fmt"
	"regexp"
	"unicode"
	"unicode/utf8"
)

// Field length limits, in characters (runes). The subject is a short
// headline; body holds any longer detail and is hidden from list
// views. The caps keep entries from turning the stream into a data
// store.
const (
	MaxTypeLen    = 64
	MaxSubjectLen = 128
	MaxBodyLen    = 2048
	MaxKeyLen     = 64
	MaxValueLen   = 256
	MaxMetadata   = 16
)

// keyPattern restricts metadata keys to lowercase slugs: start with a
// letter, then letters/digits in dash-separated groups. No leading or
// trailing dash, no double dash, no uppercase. This keeps keys tidy
// and stops Jira and jira from ever being distinct keys.
var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

func checkLen(field, value string, max int) error {
	if n := utf8.RuneCountInString(value); n > max {
		return fmt.Errorf("%s is too long: %d chars (max %d)", field, n, max)
	}
	return nil
}

func checkOneLine(field, value string) error {
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains control character U+%04X", field, r)
		}
	}
	return nil
}

func checkBody(value string) error {
	for _, r := range value {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return fmt.Errorf("body contains control character U+%04X", r)
		}
	}
	return nil
}

// ValidateKey enforces the slug rules and the length cap on a metadata
// key.
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("metadata key is required")
	}
	if err := checkLen("metadata key", key, MaxKeyLen); err != nil {
		return err
	}
	if !keyPattern.MatchString(key) {
		return fmt.Errorf(
			"invalid metadata key %q: use lowercase letters, digits, and "+
				"single dashes, starting with a letter (e.g. jira, github-pr)",
			key)
	}
	return nil
}

// ValidateMeta checks one metadata pair for storage.
func ValidateMeta(key, value string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if err := checkOneLine("metadata value", value); err != nil {
		return err
	}
	return checkLen("metadata value", value, MaxValueLen)
}

func validateMetadata(md map[string]string) error {
	if len(md) > MaxMetadata {
		return fmt.Errorf("too many metadata pairs: %d (max %d)",
			len(md), MaxMetadata)
	}
	for key, value := range md {
		if err := ValidateMeta(key, value); err != nil {
			return err
		}
	}
	return nil
}

func (r AddEntryRequest) Validate() error {
	if r.Type == "" {
		return fmt.Errorf("type is required")
	}
	if r.Subject == "" {
		return fmt.Errorf("subject is required")
	}
	if err := checkOneLine("type", r.Type); err != nil {
		return err
	}
	if err := checkLen("type", r.Type, MaxTypeLen); err != nil {
		return err
	}
	if err := checkOneLine("subject", r.Subject); err != nil {
		return err
	}
	if err := checkLen("subject", r.Subject, MaxSubjectLen); err != nil {
		return err
	}
	if err := checkLen("body", r.Body, MaxBodyLen); err != nil {
		return err
	}
	if err := checkBody(r.Body); err != nil {
		return err
	}
	return validateMetadata(r.Metadata)
}

func (r EditEntryRequest) Validate() error {
	if r.Subject == nil && r.Body == nil {
		return fmt.Errorf("nothing to edit: provide subject and/or body")
	}
	if r.Subject != nil {
		if *r.Subject == "" {
			return fmt.Errorf("subject cannot be set to empty")
		}
		if err := checkOneLine("subject", *r.Subject); err != nil {
			return err
		}
		if err := checkLen("subject", *r.Subject, MaxSubjectLen); err != nil {
			return err
		}
	}
	if r.Body != nil {
		if err := checkBody(*r.Body); err != nil {
			return err
		}
		if err := checkLen("body", *r.Body, MaxBodyLen); err != nil {
			return err
		}
	}
	return nil
}
