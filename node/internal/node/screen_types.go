package node

import (
	"regexp"
	"strconv"
)

var (
	physicalSizePattern = regexp.MustCompile(`(?i)Physical size:\s*(\d+)x(\d+)`)
	overrideSizePattern = regexp.MustCompile(`(?i)Override size:\s*(\d+)x(\d+)`)
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
