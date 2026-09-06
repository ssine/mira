package miraserver

import "fmt"

// HTTPError preserves the public status/code contract while letting handlers
// distinguish validation and authorization failures from internal errors.
type HTTPError struct {
	Status  int
	Code    string
	Message string
}

func (value *HTTPError) Error() string {
	if value.Message != "" {
		return value.Message
	}
	return fmt.Sprintf("HTTP %d", value.Status)
}
