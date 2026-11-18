package domain

import (
	"strings"
	"unicode/utf8"
)

const (
	MaxTitleLen = 200
	MaxBodyLen  = 20000
	MaxNameLen  = 100
)

// ValidateContent checks a title and body.
func ValidateContent(title, body string) error {
	if strings.TrimSpace(title) == "" {
		return Invalidf("title is required")
	}
	if utf8.RuneCountInString(title) > MaxTitleLen {
		return Invalidf("title longer than %d characters", MaxTitleLen)
	}
	if strings.TrimSpace(body) == "" {
		return Invalidf("body is required")
	}
	if utf8.RuneCountInString(body) > MaxBodyLen {
		return Invalidf("body longer than %d characters", MaxBodyLen)
	}
	return nil
}

// ValidateName checks a namespace or agent name.
func ValidateName(name string) error {
	n := strings.TrimSpace(name)
	if n == "" {
		return Invalidf("name is required")
	}
	if utf8.RuneCountInString(n) > MaxNameLen {
		return Invalidf("name longer than %d characters", MaxNameLen)
	}
	return nil
}
