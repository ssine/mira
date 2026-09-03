package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxScreenshotBytes = 10 * 1024 * 1024

var (
	physicalSizePattern = regexp.MustCompile(`(?i)Physical size:\s*(\d+)x(\d+)`)
	overrideSizePattern = regexp.MustCompile(`(?i)Override size:\s*(\d+)x(\d+)`)
	keyCodePattern      = regexp.MustCompile(`^KEYCODE_[A-Z0-9_]+$`)
)

type screenParams struct {
	Action     string `json:"action"`
	X          *int   `json:"x"`
	Y          *int   `json:"y"`
	StartX     *int   `json:"startX"`
	StartY     *int   `json:"startY"`
	EndX       *int   `json:"endX"`
	EndY       *int   `json:"endY"`
	DurationMS *int   `json:"durationMs"`
	KeyCode    any    `json:"keyCode"`
	Text       string `json:"text"`
}

type displayInfo struct {
	Width         int    `json:"width,omitempty"`
	Height        int    `json:"height,omitempty"`
	SizeOutput    string `json:"sizeOutput"`
	DensityOutput string `json:"densityOutput"`
}

func parseDisplaySize(output string) (int, int) {
	match := overrideSizePattern.FindStringSubmatch(output)
	if match == nil {
		match = physicalSizePattern.FindStringSubmatch(output)
	}
	if match == nil {
		return 0, 0
	}
	width, _ := strconv.Atoi(match[1])
	height, _ := strconv.Atoi(match[2])
	return width, height
}

func (runtime *capabilityRuntime) displayInfo(ctx context.Context) (displayInfo, error) {
	if runtime.bridge != nil {
		result, err := runtime.bridge.screen(ctx, screenParams{Action: "display"})
		if err != nil {
			return displayInfo{}, err
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return displayInfo{}, err
		}
		var display displayInfo
		if err := json.Unmarshal(encoded, &display); err != nil {
			return displayInfo{}, fmt.Errorf("decode Android bridge display: %w", err)
		}
		return display, nil
	}
	size, err := commandOutput(ctx, "wm", "size")
	if err != nil {
		return displayInfo{}, err
	}
	density, err := commandOutput(ctx, "wm", "density")
	if err != nil {
		return displayInfo{}, err
	}
	width, height := parseDisplaySize(size)
	return displayInfo{
		Width:         width,
		Height:        height,
		SizeOutput:    strings.TrimSpace(size),
		DensityOutput: strings.TrimSpace(density),
	}, nil
}

func coordinate(value *int, name string, maximum int) (int, error) {
	if value == nil || *value < 0 || (maximum > 0 && *value >= maximum) {
		return 0, fmt.Errorf("%s must be an integer inside the current display", name)
	}
	return *value, nil
}

func (runtime *capabilityRuntime) screen(ctx context.Context, params screenParams) (any, error) {
	if runtime.bridge != nil {
		return runtime.bridge.screen(ctx, params)
	}
	switch params.Action {
	case "display":
		display, err := runtime.displayInfo(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"action": "display", "width": display.Width,
			"height": display.Height, "sizeOutput": display.SizeOutput,
			"densityOutput": display.DensityOutput}, nil
	case "screenshot":
		return runtime.screenshot(ctx)
	case "hierarchy":
		return runtime.hierarchy(ctx)
	}

	display, err := runtime.displayInfo(ctx)
	if err != nil {
		return nil, err
	}
	switch params.Action {
	case "tap":
		x, err := coordinate(params.X, "x", display.Width)
		if err != nil {
			return nil, err
		}
		y, err := coordinate(params.Y, "y", display.Height)
		if err != nil {
			return nil, err
		}
		if _, err := commandOutput(ctx, "input", "touchscreen", "tap", strconv.Itoa(x), strconv.Itoa(y)); err != nil {
			return nil, err
		}
		return map[string]any{"action": "tap", "x": x, "y": y, "accepted": true}, nil
	case "swipe":
		return runtime.swipe(ctx, params, display)
	case "key":
		keyCode, err := validatedKeyCode(params.KeyCode)
		if err != nil {
			return nil, err
		}
		if _, err := commandOutput(ctx, "input", "keyevent", keyCode); err != nil {
			return nil, err
		}
		return map[string]any{"action": "key", "keyCode": keyCode, "accepted": true}, nil
	case "text":
		if params.Text == "" || len(params.Text) > 4096 || strings.ContainsRune(params.Text, 0) {
			return nil, fmt.Errorf("text must contain between 1 and 4096 bytes")
		}
		encoded := strings.ReplaceAll(params.Text, " ", "%s")
		if _, err := commandOutput(ctx, "input", "text", encoded); err != nil {
			return nil, err
		}
		return map[string]any{
			"action": "text", "characters": len([]rune(params.Text)), "accepted": true,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported screen action: %s", params.Action)
	}
}

func (runtime *capabilityRuntime) screenshot(ctx context.Context) (any, error) {
	display, err := runtime.displayInfo(ctx)
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, "screencap", "-p")
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("screencap failed: %s", strings.TrimSpace(output.String()))
	}
	png := output.Bytes()
	if len(png) > maxScreenshotBytes {
		return nil, fmt.Errorf("Android screenshot exceeds 10 MiB")
	}
	if len(png) < 8 || !bytes.Equal(png[:8], []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}) {
		return nil, fmt.Errorf("screencap did not return a PNG image")
	}
	return map[string]any{
		"action":     "screenshot",
		"mimeType":   "image/png",
		"encoding":   "base64",
		"content":    base64.StdEncoding.EncodeToString(png),
		"bytes":      len(png),
		"width":      display.Width,
		"height":     display.Height,
		"capturedAt": time.Now().UTC().Format(time.RFC3339Nano),
	}, nil
}

func (runtime *capabilityRuntime) hierarchy(ctx context.Context) (any, error) {
	temporary := fmt.Sprintf("/data/local/tmp/codex-ui-%d.xml", time.Now().UnixNano())
	defer os.Remove(temporary)
	var content []byte
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		_ = os.Remove(temporary)
		output, err := commandOutput(ctx, "uiautomator", "dump", temporary)
		if err == nil {
			content, err = os.ReadFile(temporary)
		}
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = fmt.Errorf("read UI hierarchy: %w (%s)", err, strings.TrimSpace(output))
		if attempt < 4 && !sleepContext(ctx, 250*time.Millisecond) {
			return nil, ctx.Err()
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	if len(content) > maxFileBytes {
		return nil, fmt.Errorf("UI hierarchy exceeds 4 MiB")
	}
	return map[string]any{
		"action": "hierarchy", "format": "uiautomator-xml", "content": string(content),
	}, nil
}

func (runtime *capabilityRuntime) swipe(
	ctx context.Context,
	params screenParams,
	display displayInfo,
) (any, error) {
	startX, err := coordinate(params.StartX, "startX", display.Width)
	if err != nil {
		return nil, err
	}
	startY, err := coordinate(params.StartY, "startY", display.Height)
	if err != nil {
		return nil, err
	}
	endX, err := coordinate(params.EndX, "endX", display.Width)
	if err != nil {
		return nil, err
	}
	endY, err := coordinate(params.EndY, "endY", display.Height)
	if err != nil {
		return nil, err
	}
	duration := 300
	if params.DurationMS != nil {
		duration = *params.DurationMS
	}
	if duration < 1 || duration > 60000 {
		return nil, fmt.Errorf("durationMs must be between 1 and 60000")
	}
	arguments := []string{
		"touchscreen", "swipe", strconv.Itoa(startX), strconv.Itoa(startY),
		strconv.Itoa(endX), strconv.Itoa(endY), strconv.Itoa(duration),
	}
	if _, err := commandOutput(ctx, "input", arguments...); err != nil {
		return nil, err
	}
	return map[string]any{
		"action": "swipe", "startX": startX, "startY": startY,
		"endX": endX, "endY": endY, "durationMs": duration, "accepted": true,
	}, nil
}

func validatedKeyCode(value any) (string, error) {
	switch typed := value.(type) {
	case string:
		if !keyCodePattern.MatchString(typed) {
			return "", fmt.Errorf("keyCode must match KEYCODE_*")
		}
		return typed, nil
	case float64:
		integer := int(typed)
		if typed != float64(integer) || integer < 0 || integer > 999 {
			return "", fmt.Errorf("keyCode integer must be between 0 and 999")
		}
		return strconv.Itoa(integer), nil
	default:
		return "", fmt.Errorf("keyCode must be an Android keycode integer or KEYCODE_* name")
	}
}
