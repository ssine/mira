package imports

import (
	"context"
	"errors"
	"fmt"

	"github.com/ssine/mira/node/internal/miraserver/channel"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

// Error is an alias so the parent HTTP server recognizes import failures
// without a package-specific adapter.
type Error = foundation.HTTPError

func importError(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

func invalidHistory(message string) *Error {
	return importError(409, "invalid_history_base", message)
}

func errorCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancelled"
	}
	var failure *Error
	if errors.As(err, &failure) && failure.Code != "" {
		return failure.Code
	}
	var channelFailure *channel.Error
	if errors.As(err, &channelFailure) && channelFailure.Code != "" {
		return channelFailure.Code
	}
	type coded interface{ Code() string }
	if failure, ok := err.(coded); ok && failure.Code() != "" {
		return failure.Code()
	}
	return "import_failed"
}

func wrapInvalidRecord(index int64, kind string) error {
	return importError(409, kind, fmt.Sprintf("Invalid rollout envelope at record %d", index))
}
