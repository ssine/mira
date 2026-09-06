package miraserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	serverchannel "github.com/ssine/mira/node/internal/miraserver/channel"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	serverimports "github.com/ssine/mira/node/internal/miraserver/imports"
)

var (
	codexSessionsPattern = regexp.MustCompile(`(?i)^/v1/nodes/([0-9a-f-]{36})/codex-sessions$`)
	sessionImportPattern = regexp.MustCompile(`(?i)^/v1/nodes/([0-9a-f-]{36})/codex-session-imports$`)
)

func (server *Server) routeImports(ctx context.Context, response http.ResponseWriter, request *http.Request) (bool, error) {
	if match := pathMatch(codexSessionsPattern, request.URL.Path); request.Method == http.MethodGet && match != nil {
		principal, err := server.authorize(ctx, response, request, "admin", authOptions{})
		if err != nil || principal == nil {
			return true, err
		}
		storeID := request.URL.Query().Get("storeId")
		if storeID == "" {
			storeID = serverimports.DefaultStoreID
		}
		result, err := server.imports.ScanCodexSessions(ctx, principal, match[1], request, storeID)
		if err != nil {
			return true, err
		}
		return true, writeJSON(response, 200, result)
	}
	if match := pathMatch(sessionImportPattern, request.URL.Path); request.Method == http.MethodPost && match != nil {
		principal, err := server.authorize(ctx, response, request, "admin", authOptions{})
		if err != nil || principal == nil {
			return true, err
		}
		body := map[string]any{}
		if err := foundation.ReadJSON(request, &body, server.config.Foundation.MaxBodyBytes); err != nil {
			return true, err
		}
		input := serverimports.Request{Principal: principal, NodeID: match[1], Body: body, HTTP: request}
		if !strings.Contains(strings.ToLower(request.Header.Get("Accept")), "application/x-ndjson") {
			result, err := server.imports.ImportCodexSession(ctx, input)
			if err != nil {
				return true, err
			}
			return true, writeJSON(response, result.Status, result.Body)
		}
		return true, server.streamSessionImport(ctx, response, input)
	}
	return false, nil
}

type importOutcome struct {
	result serverimports.Result
	err    error
}

func (server *Server) streamSessionImport(ctx context.Context, response http.ResponseWriter, input serverimports.Request) error {
	response.Header().Set("Content-Type", "application/x-ndjson")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("X-Accel-Buffering", "no")
	response.WriteHeader(http.StatusOK)
	flusher, _ := response.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	progress := make(chan map[string]any, 16)
	completed := make(chan importOutcome, 1)
	input.Progress = serverimports.Context{OnProgress: func(value map[string]any) {
		select {
		case progress <- value:
		case <-ctx.Done():
		}
	}}
	go func() {
		result, err := server.imports.ImportCodexSession(ctx, input)
		completed <- importOutcome{result: result, err: err}
	}()
	encoder := json.NewEncoder(response)
	heartbeat := time.NewTicker(5 * time.Second)
	defer heartbeat.Stop()
	emit := func(value map[string]any) error {
		if err := encoder.Encode(value); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	for {
		select {
		case update := <-progress:
			event := map[string]any{"type": "progress"}
			for key, value := range update {
				event[key] = value
			}
			if err := emit(event); err != nil {
				return nil
			}
		case outcome := <-completed:
			if outcome.err != nil {
				message, code := importFailure(outcome.err)
				return emit(map[string]any{"type": "error", "error": message, "code": code})
			}
			typeName := "complete"
			if outcome.result.Status != http.StatusOK {
				typeName = "error"
			}
			event := map[string]any{"type": typeName}
			if body, ok := outcome.result.Body.(map[string]any); ok {
				for key, value := range body {
					event[key] = value
				}
			}
			return emit(event)
		case <-heartbeat.C:
			if err := emit(map[string]any{"type": "heartbeat"}); err != nil {
				return nil
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func importFailure(err error) (string, string) {
	code := "import_failed"
	var own *HTTPError
	if errors.As(err, &own) && own.Code != "" {
		code = own.Code
	}
	var base *foundation.HTTPError
	if errors.As(err, &base) && base.Code != "" {
		code = base.Code
	}
	var channelError *serverchannel.Error
	if errors.As(err, &channelError) && channelError.Code != "" {
		code = channelError.Code
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		code = "cancelled"
	}
	return err.Error(), code
}
