//go:build !windows && !android

package node

import "context"

func systemProcessList(ctx context.Context) (any, error) {
	const maximum = 1024 * 1024
	output, truncated, err := commandOutputLimited(ctx, maximum, "ps", "-eo", "pid,ppid,stat,%cpu,%mem,etime,comm,args", "--no-headers")
	if err != nil {
		return nil, err
	}
	return map[string]any{"format": "ps", "output": output, "truncated": truncated, "maxBytes": maximum}, nil
}
