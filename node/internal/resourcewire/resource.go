// Package resourcewire defines the bounded file data-plane contract.
package resourcewire

import "net/http"

const Protocol = "mira-file-v1"
const FrameBytes = 64 * 1024

type Request struct {
	CacheIdentity    string      `json:"-"`
	CacheDirectory   string      `json:"-"`
	Path             string      `json:"path"`
	DiagnosticURL    string      `json:"diagnosticUrl,omitempty"`
	Root             string      `json:"root,omitempty"`
	ArchiveVersion   string      `json:"archiveVersion,omitempty"`
	Archive          bool        `json:"archive,omitempty"`
	ArchiveEntry     string      `json:"archiveEntry,omitempty"`
	EntryID          *int        `json:"entryId,omitempty"`
	ExecutionContext string      `json:"executionContext,omitempty"`
	UserSessionID    *uint32     `json:"userSessionId,omitempty"`
	Method           string      `json:"method"`
	Headers          http.Header `json:"headers,omitempty"`
}
type Response struct {
	Status  int         `json:"status"`
	Headers http.Header `json:"headers"`
}
