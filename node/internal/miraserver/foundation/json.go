package foundation

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

type HTTPError struct {
	Status  int
	Code    string
	Message string
}

func (err *HTTPError) Error() string { return err.Message }

func ReadJSON(request *http.Request, destination any, maxBytes int64) error {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBodyBytes
	}
	payload, err := io.ReadAll(io.LimitReader(request.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("read request body: %w", err)
	}
	if int64(len(payload)) > maxBytes {
		return &HTTPError{Status: http.StatusRequestEntityTooLarge, Code: "body_too_large", Message: "request body exceeds 64 MiB"}
	}
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	if err := json.Unmarshal(payload, destination); err != nil {
		return &HTTPError{Status: http.StatusBadRequest, Code: "invalid_json", Message: "request body must be valid JSON"}
	}
	return nil
}

func WriteJSON(response http.ResponseWriter, status int, value any, headers http.Header) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("serialize JSON response: %w", err)
	}
	for name, values := range headers {
		for _, value := range values {
			response.Header().Add(name, value)
		}
	}
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	if _, err := response.Write(payload); err != nil {
		return fmt.Errorf("write JSON response: %w", err)
	}
	return nil
}

func WriteErrorJSON(response http.ResponseWriter, status int, message, code string) error {
	return WriteJSON(response, status, map[string]string{"error": message, "code": code}, nil)
}
