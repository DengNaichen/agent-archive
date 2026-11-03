// Package fault marks messages deliberately safe to display at the CLI boundary.
// Parser, filesystem, SQL and transport details remain in wrapped internal errors.
package fault

import "errors"

type Error struct{ text string }

func (e *Error) Error() string { return e.text }
func New(text string) error    { return &Error{text: text} }
func Message(err error) string {
	var public *Error
	if errors.As(err, &public) {
		return public.text
	}
	return "operation failed; check configuration, local file access and connectivity; sensitive details withheld"
}
