package views

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/jackc/pgx/v5"
)

type historyImage struct {
	index int
	url   string
}

// Only typed model-facing content is evidence of an image in the conversation.
// Paths, event notifications, JSON-looking tool text and generated file names
// must never cause the console to read a Node's current filesystem.
func historyImages(record map[string]any) []historyImage {
	if stringValue(record["type"]) != "response_item" {
		return nil
	}
	payload := object(record["payload"])
	var content any
	switch stringValue(payload["type"]) {
	case "message":
		if !visibleResponseMessage(payload) {
			return nil
		}
		content = payload["content"]
	case "function_call_output", "custom_tool_call_output":
		content = payload["output"]
	default:
		return nil
	}
	var images []historyImage
	for index, part := range array(content) {
		item := object(part)
		if stringValue(item["type"]) == "input_image" {
			if data := imageDataURL(item["image_url"]); data != "" {
				images = append(images, historyImage{index, data})
			}
		}
	}
	return images
}

func referenceTranscriptImages(trace []map[string]any, storeID, threadID string, generation int64) {
	for _, item := range trace {
		if item["kind"] != "image" {
			continue
		}
		sequence, _ := safeInteger(item["sourceItemSeq"])
		item["image"] = map[string]any{"href": fmt.Sprintf("/v1/codex/threads/%s/transcript/image?storeId=%s&generation=%d&itemSeq=%d&index=%v",
			url.PathEscape(threadID), url.QueryEscape(storeID), generation, sequence, item["imageIndex"])}
	}
}

// GetTranscriptImage reads one immutable content record in the active generation.
// Images are fetched independently of tool details and other transcript pages.
func (service *Service) GetTranscriptImage(ctx context.Context, storeID, threadID string, generation, sequence, index int64) (Result, error) {
	if generation < 1 || sequence < 1 || index < 0 {
		return errorResult(400, "invalid image reference", "invalid_request"), nil
	}
	var raw []byte
	err := service.pool.QueryRow(ctx, `SELECT events.payload FROM codex_thread_events events
  JOIN codex_thread_projections projections ON projections.store_id=events.store_id AND projections.thread_id=events.thread_id
   AND projections.active_generation=events.generation
  WHERE events.store_id=$1 AND events.thread_id=$2 AND events.generation=$3 AND events.item_seq=$4
   AND events.item_seq<=projections.item_count
   AND NOT EXISTS(SELECT 1 FROM mira_thread_actions WHERE store_id=$1 AND thread_id=$2 AND action='delete')`,
		storeID, threadID, generation, sequence).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return errorResult(404, "history image not found", "not_found"), nil
	}
	if err != nil {
		return Result{}, err
	}
	record, err := decodeObject(raw)
	if err != nil {
		return Result{}, err
	}
	for _, image := range historyImages(record) {
		if int64(image.index) == index {
			return Result{Status: 200, Body: map[string]any{"url": image.url}}, nil
		}
	}
	return errorResult(404, "history image not found", "not_found"), nil
}
