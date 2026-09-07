package node

import (
	"fmt"
	"strings"
)

const (
	executionContextUser   = "user"
	executionContextSystem = "system"
)

type executionRequest struct {
	Context       string
	UserSessionID *uint32
}

type executionIdentity struct {
	Context       string
	OSIdentity    string
	UserSessionID *uint32
	token         uintptr
	environment   []string
	closeToken    bool
}

func validateExecutionRequest(request executionRequest) error {
	if request.Context != "" && request.Context != executionContextUser && request.Context != executionContextSystem {
		return fmt.Errorf("executionContext must be user or system")
	}
	if request.UserSessionID != nil && request.Context != "" && request.Context != executionContextUser {
		return fmt.Errorf("userSessionId requires executionContext user")
	}
	return nil
}

func executionMetadata(identity *executionIdentity) map[string]any {
	result := map[string]any{
		"executionContext": identity.Context,
		"osIdentity":       identity.OSIdentity,
	}
	if identity.UserSessionID != nil {
		result["userSessionId"] = *identity.UserSessionID
	}
	return result
}

func attachExecutionMetadata(value any, identity *executionIdentity) any {
	result, ok := value.(map[string]any)
	if !ok {
		return value
	}
	for name, item := range executionMetadata(identity) {
		result[name] = item
	}
	return result
}

func mergeExecutionEnvironment(base []string, overrides map[string]string, caseInsensitive bool) []string {
	result := append([]string(nil), base...)
	indexes := make(map[string]int, len(result))
	key := func(name string) string {
		if caseInsensitive {
			return strings.ToUpper(name)
		}
		return name
	}
	for index, item := range result {
		if separator := strings.IndexByte(item, '='); separator > 0 {
			indexes[key(item[:separator])] = index
		}
	}
	for name, value := range overrides {
		item := name + "=" + value
		if index, exists := indexes[key(name)]; exists {
			result[index] = item
		} else {
			indexes[key(name)] = len(result)
			result = append(result, item)
		}
	}
	return result
}
