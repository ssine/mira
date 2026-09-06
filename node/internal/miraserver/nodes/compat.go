package nodes

import (
	"context"
	"net/http"
)

// The Node-suffixed names mirror the former JavaScript module exports and keep
// call sites self-explanatory where several Server services are in scope.
func (service *Service) RegisterNode(ctx context.Context, nodeID string, body map[string]any) (Result, error) {
	return service.Register(ctx, nodeID, body)
}

func (service *Service) HeartbeatNode(ctx context.Context, nodeID string, body map[string]any) (Result, error) {
	return service.Heartbeat(ctx, nodeID, body)
}

func (service *Service) ListNodes(ctx context.Context, includeRevoked bool) ([]Node, error) {
	return service.List(ctx, includeRevoked)
}

func (service *Service) GetNode(ctx context.Context, nodeID string, includeRevoked bool) (*Node, error) {
	return service.Get(ctx, nodeID, includeRevoked)
}

func (service *Service) ResolveNode(ctx context.Context, selector string, includeRevoked bool) (Result, error) {
	return service.Resolve(ctx, selector, includeRevoked)
}

func NodeSummary(node Node) map[string]any { return Summary(node) }

func (service *Service) SetNodeMetadata(ctx context.Context, request *http.Request, principal *Principal, nodeID string, body map[string]any) (Result, error) {
	return service.SetMetadata(ctx, request, principal, nodeID, body)
}

func (service *Service) SetNodeChannelStatus(ctx context.Context, nodeID string, status any) error {
	return service.SetChannelStatus(ctx, nodeID, status)
}

func (service *Service) RevokeNode(ctx context.Context, request *http.Request, principal *Principal, nodeID string, reason *string) (Result, error) {
	return service.Revoke(ctx, request, principal, nodeID, reason)
}

func (service *Service) RestoreNode(ctx context.Context, request *http.Request, principal *Principal, nodeID string, body map[string]any) (Result, error) {
	return service.Restore(ctx, request, principal, nodeID, body)
}
