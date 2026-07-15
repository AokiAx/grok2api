package bootstrap

import (
	"bytes"
	"errors"
	"io"
	"unicode/utf8"
)

var ErrInvalidPasswordInput = errors.New("invalid admin password input")

// ReadPasswordStdin reads exactly one password line. It removes only the line
// ending and preserves all other bytes, including leading and trailing spaces.
func ReadPasswordStdin(input io.Reader) (string, error) {
	if input == nil {
		return "", ErrInvalidPasswordInput
	}
	data, err := io.ReadAll(io.LimitReader(input, maximumAdminPasswordBytes+3))
	if err != nil {
		return "", err
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return "", ErrInvalidPasswordInput
	}
	newline := bytes.IndexByte(data, '\n')
	if newline >= 0 {
		if newline != len(data)-1 {
			return "", ErrInvalidPasswordInput
		}
		data = data[:newline]
		if len(data) > 0 && data[len(data)-1] == '\r' {
			data = data[:len(data)-1]
		}
	}
	if bytes.IndexByte(data, '\r') >= 0 {
		return "", ErrInvalidPasswordInput
	}
	if len(data) > maximumAdminPasswordBytes {
		return "", ErrInvalidPasswordInput
	}
	return string(data), nil
}
