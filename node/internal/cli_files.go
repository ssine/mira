package node

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type cliFileProgress struct {
	writer io.Writer
	label  string
	total  int64
	last   time.Time
}

func (progress *cliFileProgress) update(total int64) {
	progress.total = total
	if progress.writer != nil && time.Since(progress.last) >= time.Second {
		fmt.Fprintf(progress.writer, "\r%s %d bytes", progress.label, total)
		progress.last = time.Now()
	}
}

func (progress *cliFileProgress) finish() {
	if progress.writer != nil {
		fmt.Fprintf(progress.writer, "\r%s %d bytes\n", progress.label, progress.total)
	}
}

// A Windows transfer keeps using the interactive session selected for its first
// operation instead of selecting a different active user between chunks.
func pinFileExecutionSession(params, result map[string]any) {
	if result["executionContext"] == executionContextUser && result["userSessionId"] != nil {
		params["executionContext"] = executionContextUser
		params["userSessionId"] = result["userSessionId"]
	}
}

func (client *cliClient) writeFileChunks(ctx context.Context, selector string, params map[string]any, source io.Reader, writer io.Writer) (any, error) {
	node, err := client.resolveNode(ctx, selector)
	if err != nil {
		return nil, err
	}
	nodeID := selectorValue(node, "nodeId")
	capabilities, _ := node["capabilities"].(map[string]any)
	chunked := capabilities["fileChunkedWrite"] == true
	// Look ahead by one byte so an older Node rejects a large input before we
	// create or truncate its destination. Even stdin stays bounded in memory.
	reader := source
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !chunked {
		buffered := bufio.NewReaderSize(source, maxFileBytes+1)
		preview, err := buffered.Peek(maxFileBytes + 1)
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("read upload input: %w", err)
		}
		if len(preview) > maxFileBytes {
			return nil, fmt.Errorf("target Node requires fileChunkedWrite for large uploads; upgrade it first")
		}
		reader = buffered
	}
	progress := cliFileProgress{writer: writer, label: "Uploaded"}
	defer progress.finish()
	buffer := make([]byte, maxFileBytes)
	var total int64
	var result map[string]any
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("upload stopped after %d confirmed bytes; remote file may be incomplete: %w", total, err)
		}
		count, readErr := io.ReadFull(reader, buffer)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("read upload input after %d confirmed bytes; remote file may be incomplete: %w", total, readErr)
		}
		if count == 0 && result != nil {
			break
		}
		params["encoding"] = "base64"
		params["content"] = base64.StdEncoding.EncodeToString(buffer[:count])
		params["append"], params["offset"] = total > 0, total
		// Never retry a lost response: the remote write may already have happened.
		value, err := client.invokeNode(ctx, nodeID, "file", params)
		if err != nil {
			return nil, fmt.Errorf("upload failed after %d confirmed bytes; remote file may be incomplete: %w", total, err)
		}
		var ok bool
		result, ok = value.(map[string]any)
		if !ok || result["bytesWritten"] != float64(count) {
			return nil, fmt.Errorf("unexpected file write response after %d confirmed bytes; remote file may be incomplete", total)
		}
		pinFileExecutionSession(params, result)
		total += int64(count)
		progress.update(total)
		if readErr != nil || !chunked {
			break
		}
	}
	result["bytesWritten"] = total
	return result, nil
}

func fileResponseBytes(result map[string]any, name string) (int64, error) {
	value, ok := result[name].(float64)
	if !ok || value < 0 || value > 9_007_199_254_740_991 || value != float64(int64(value)) {
		return 0, fmt.Errorf("unexpected file read %s", name)
	}
	return int64(value), nil
}

func (client *cliClient) readFileChunks(ctx context.Context, selector string, params map[string]any, output string, offset, length int64, writer io.Writer) (any, error) {
	node, err := client.resolveNode(ctx, selector)
	if err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(output)
	if err != nil {
		return nil, err
	}
	// Publish only a complete download. Errors and cancellation preserve an
	// existing local file and remove the temporary partial download.
	file, err := os.CreateTemp(filepath.Dir(absolute), ".mira-download-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	progress := cliFileProgress{writer: writer, label: "Downloaded"}
	defer progress.finish()
	var total, size int64
	var result map[string]any
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunkLength := int64(maxFileBytes)
		if length > 0 && length-total < chunkLength {
			chunkLength = length - total
		}
		params["encoding"], params["offset"], params["length"] = "base64", offset+total, chunkLength
		value, err := client.invokeNode(ctx, selectorValue(node, "nodeId"), "file", params)
		if err != nil {
			return nil, err
		}
		view, ok := value.(map[string]any)
		if !ok || view["encoding"] != "base64" {
			return nil, fmt.Errorf("unexpected file read response")
		}
		count, err := fileResponseBytes(view, "bytesRead")
		if err != nil {
			return nil, err
		}
		chunkOffset, err := fileResponseBytes(view, "offset")
		if err != nil || chunkOffset != offset+total {
			return nil, fmt.Errorf("unexpected file read offset")
		}
		chunkSize, err := fileResponseBytes(view, "size")
		if err != nil {
			return nil, err
		}
		if result == nil {
			size = chunkSize
		} else if size != chunkSize {
			return nil, fmt.Errorf("remote file size changed during download")
		}
		content, err := base64.StdEncoding.DecodeString(selectorValue(view, "content"))
		if err != nil {
			return nil, err
		}
		eof, ok := view["eof"].(bool)
		if !ok || count != int64(len(content)) || count > chunkLength || (count == 0 && !eof) || eof != (chunkOffset+count >= size) {
			return nil, fmt.Errorf("unexpected file read chunk")
		}
		if _, err := file.Write(content); err != nil {
			return nil, err
		}
		pinFileExecutionSession(params, view)
		total += count
		progress.update(total)
		result = view
		if eof || (length > 0 && total == length) {
			break
		}
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.Rename(file.Name(), absolute); err != nil {
		return nil, err
	}
	delete(result, "content")
	result["offset"], result["bytesRead"] = offset, total
	result["localPath"], result["sourceNodeId"] = absolute, selectorValue(node, "nodeId")
	return result, nil
}
