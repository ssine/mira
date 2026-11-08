package node

import (
	"errors"

	"github.com/ssine/mira/node/internal/accountegress"
)

func (client *controlClient) handleAccountEgress(params map[string]any) (any, error) {
	id, _ := params["nodeAccountId"].(string)
	if id == "" {
		return nil, errors.New("nodeAccountId is required")
	}
	action, _ := params["action"].(string)
	switch action {
	case "configure":
		spec, public, err := accountegress.ParsePublic(params["egress"])
		if err != nil {
			return nil, err
		}
		if _, err = client.egress.Configure(id, *spec); err != nil {
			return nil, err
		}
		return map[string]any{"configured": true, "egress": public}, nil
	case "delete":
		if err := client.egress.Delete(id); err != nil {
			return nil, err
		}
		return map[string]any{"configured": false}, nil
	case "status":
		return client.egress.Status(id)
	default:
		return nil, errors.New("unknown account egress action")
	}
}
