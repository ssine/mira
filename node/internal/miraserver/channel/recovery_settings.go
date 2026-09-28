package channel

import (
	"encoding/json"
	"errors"
)

// RecoveryResumeSettings preserves the recorded approval and sandbox policy.
func RecoveryResumeSettings(raw []byte) (map[string]any, error) {
	var err error
	var record map[string]any
	if err = json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	p := recoveryObject(record["payload"])
	result := map[string]any{}
	for from, to := range map[string]string{"model": "model", "cwd": "cwd", "approval_policy": "approvalPolicy", "effort": "reasoningEffort"} {
		if value := p[from]; value != nil {
			result[to] = value
		}
	}
	policy := recoveryObject(p["sandbox_policy"])
	switch policy["type"] {
	case "danger-full-access":
		result["sandbox"] = "danger-full-access"
	case "read-only":
		result["sandbox"] = "read-only"
	case "workspace-write":
		result["sandbox"] = "workspace-write"
		result["config"] = map[string]any{"sandbox_workspace_write": policy}
	default:
		return nil, errors.New("当前沙箱设置需要手动恢复")
	}
	if result["approvalPolicy"] == nil {
		return nil, errors.New("缺少原轮次的审批设置，请手动继续")
	}
	// App Server exposes reasoning effort through config on resume.
	if effort := result["reasoningEffort"]; effort != nil {
		delete(result, "reasoningEffort")
		config, _ := result["config"].(map[string]any)
		if config == nil {
			config = map[string]any{}
		}
		config["model_reasoning_effort"] = effort
		result["config"] = config
	}
	return result, nil
}

func recoveryObject(value any) map[string]any { result, _ := value.(map[string]any); return result }
