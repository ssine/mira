package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxFileBytes = 4 * 1024 * 1024

type fileParams struct {
	Action      string `json:"action"`
	Path        string `json:"path"`
	Destination string `json:"destination"`
	Content     string `json:"content"`
	Encoding    string `json:"encoding"`
	Offset      int64  `json:"offset"`
	Length      *int64 `json:"length"`
	Recursive   bool   `json:"recursive"`
	Overwrite   *bool  `json:"overwrite"`
}

func pathContained(root string, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))))
}

func resolveExistingAncestor(input string) (string, string, error) {
	if input == "" || !filepath.IsAbs(input) || strings.ContainsRune(input, 0) {
		return "", "", fmt.Errorf("path must be a non-empty absolute Android path")
	}
	candidate := filepath.Clean(input)
	current := candidate
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			suffix, err := filepath.Rel(current, candidate)
			if err != nil {
				return "", "", err
			}
			return candidate, filepath.Clean(filepath.Join(resolved, suffix)), nil
		}
		if !os.IsNotExist(err) && !os.IsPermission(err) {
			return "", "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", "", err
		}
		current = parent
	}
}

func (runtime *capabilityRuntime) authorize(input string, allowRoot bool) (string, error) {
	if len(input) > 32768 {
		return "", fmt.Errorf("path exceeds 32768 bytes")
	}
	candidate, resolved, err := resolveExistingAncestor(input)
	if err != nil {
		return "", err
	}
	rootIndex := -1
	for index, root := range runtime.roots {
		if pathContained(root, candidate) {
			rootIndex = index
			break
		}
	}
	if rootIndex == -1 {
		return "", fmt.Errorf("path is outside configured Android roots: %s", candidate)
	}
	if !allowRoot && candidate == runtime.roots[rootIndex] {
		return "", fmt.Errorf("operation on an allowed root is forbidden")
	}
	if !pathContained(runtime.realRoots[rootIndex], resolved) {
		return "", fmt.Errorf("path resolves outside configured Android roots: %s", candidate)
	}
	return resolved, nil
}

func statView(path string, value os.FileInfo) map[string]any {
	typeName := "other"
	switch {
	case value.Mode().IsDir():
		typeName = "directory"
	case value.Mode().IsRegular():
		typeName = "file"
	case value.Mode()&os.ModeSymlink != 0:
		typeName = "symlink"
	}
	return map[string]any{
		"path": path, "type": typeName, "size": value.Size(),
		"mode": value.Mode().String(), "modifiedAt": value.ModTime().UTC().Format(time.RFC3339Nano),
	}
}

func (runtime *capabilityRuntime) file(params fileParams) (any, error) {
	if params.Action == "roots" {
		roots := make([]map[string]string, 0, len(runtime.roots))
		for index, root := range runtime.roots {
			roots = append(roots, map[string]string{"configured": root, "resolved": runtime.realRoots[index]})
		}
		return map[string]any{"roots": roots}, nil
	}
	allowRoot := params.Action != "move" && params.Action != "remove"
	target, err := runtime.authorize(params.Path, allowRoot)
	if err != nil {
		return nil, err
	}
	switch params.Action {
	case "stat":
		value, err := os.Lstat(target)
		if err != nil {
			return nil, err
		}
		return statView(target, value), nil
	case "list":
		return runtime.listFiles(target)
	case "read":
		return runtime.readFile(target, params)
	case "write":
		return runtime.writeFile(target, params)
	case "mkdir":
		if params.Recursive {
			err = os.MkdirAll(target, 0750)
		} else {
			err = os.Mkdir(target, 0750)
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"path": target, "created": true}, nil
	case "move":
		return runtime.moveFile(target, params)
	case "remove":
		if params.Recursive {
			err = os.RemoveAll(target)
		} else {
			err = os.Remove(target)
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"path": target, "removed": true}, nil
	default:
		return nil, fmt.Errorf("unsupported file action: %s", params.Action)
	}
}

func (runtime *capabilityRuntime) listFiles(target string) (any, error) {
	entries, err := os.ReadDir(target)
	if err != nil {
		return nil, err
	}
	if len(entries) > 10000 {
		return nil, fmt.Errorf("directory contains more than 10,000 entries")
	}
	result := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		entryPath := filepath.Join(target, entry.Name())
		value, err := os.Lstat(entryPath)
		if err != nil {
			return nil, err
		}
		result = append(result, statView(entryPath, value))
	}
	return map[string]any{"path": target, "entries": result}, nil
}

func (runtime *capabilityRuntime) readFile(target string, params fileParams) (any, error) {
	value, err := os.Stat(target)
	if err != nil {
		return nil, err
	}
	if !value.Mode().IsRegular() {
		return nil, fmt.Errorf("read target is not a regular file")
	}
	if params.Offset < 0 {
		return nil, fmt.Errorf("offset must be non-negative")
	}
	length := int64(maxFileBytes)
	if params.Length != nil {
		length = *params.Length
	} else if remaining := value.Size() - params.Offset; remaining < length {
		length = remaining
	}
	if length < 0 {
		length = 0
	}
	if length > maxFileBytes {
		return nil, fmt.Errorf("read exceeds 4 MiB")
	}
	file, err := os.Open(target)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content := make([]byte, length)
	count, err := file.ReadAt(content, params.Offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	content = content[:count]
	encoding := "utf8"
	encoded := string(content)
	if params.Encoding == "base64" {
		encoding = "base64"
		encoded = base64.StdEncoding.EncodeToString(content)
	}
	return map[string]any{
		"path": target, "offset": params.Offset, "bytesRead": count, "size": value.Size(),
		"encoding": encoding, "content": encoded, "eof": params.Offset+int64(count) >= value.Size(),
	}, nil
}

func (runtime *capabilityRuntime) writeFile(target string, params fileParams) (any, error) {
	content := []byte(params.Content)
	var err error
	if params.Encoding == "base64" {
		content, err = base64.StdEncoding.DecodeString(params.Content)
		if err != nil {
			return nil, fmt.Errorf("decode base64 content: %w", err)
		}
	}
	if len(content) > maxFileBytes {
		return nil, fmt.Errorf("write exceeds 4 MiB")
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if params.Overwrite != nil && !*params.Overwrite {
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	file, err := os.OpenFile(target, flags, 0640)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	count, err := file.Write(content)
	if err != nil {
		return nil, err
	}
	return map[string]any{"path": target, "bytesWritten": count}, nil
}

func (runtime *capabilityRuntime) moveFile(target string, params fileParams) (any, error) {
	destination, err := runtime.authorize(params.Destination, false)
	if err != nil {
		return nil, err
	}
	_, statErr := os.Lstat(destination)
	if statErr == nil && (params.Overwrite == nil || !*params.Overwrite) {
		return nil, fmt.Errorf("destination already exists: %s", destination)
	}
	if statErr != nil && !os.IsNotExist(statErr) {
		return nil, statErr
	}
	if statErr == nil && params.Overwrite != nil && *params.Overwrite {
		if err := os.RemoveAll(destination); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(target, destination); err != nil {
		return nil, err
	}
	return map[string]any{"path": target, "destination": destination}, nil
}
