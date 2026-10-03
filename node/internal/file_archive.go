package node

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Directory pages retain only one page; cursor is an ordinal in OS enumeration.
// A changed directory requires restarting at cursor 0.
func (runtime *capabilityRuntime) listFilePage(target string, params fileParams) (any, error) {
	if params.Cursor < 0 || params.PageSize < 1 || params.PageSize > 1000 {
		return nil, fmt.Errorf("invalid directory page")
	}
	dir, err := os.Open(target)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("not a directory")
	}
	remaining := params.Cursor
	for remaining > 0 {
		n := min(remaining, 1000)
		entries, e := dir.ReadDir(n)
		remaining -= len(entries)
		if e != nil {
			if e == io.EOF {
				break
			}
			return nil, e
		}
	}
	entries, err := dir.ReadDir(params.PageSize + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	more := len(entries) > params.PageSize
	if more {
		entries = entries[:params.PageSize]
	}
	result := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		info, e := entry.Info()
		if e != nil {
			continue
		}
		item := statView(filepath.Join(target, entry.Name()), info)
		item["name"] = entry.Name()
		result = append(result, item)
	}
	return map[string]any{"path": target, "entries": result, "nextCursor": params.Cursor + len(entries), "hasMore": more, "modifiedAt": info.ModTime()}, nil
}

func safeArchivePath(name string) bool {
	return !strings.ContainsAny(name, "\\\x00") && !strings.HasPrefix(name, "/") && !strings.Contains(name, ":") && (name == "" || (path.Clean(strings.TrimSuffix(name, "/")) == strings.TrimSuffix(name, "/") && name != ".." && !strings.HasPrefix(name, "../")))
}

// Bound the central directory before archive/zip allocates its index. ZIP64
// counts are still supported when their central directory fits this budget.
func openPreviewZIP(target string) (*zip.Reader, *os.File, error) {
	f, err := os.Open(target)
	if err != nil {
		return nil, nil, err
	}
	fail := func(e error) (*zip.Reader, *os.File, error) { f.Close(); return nil, nil, e }
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fail(fmt.Errorf("ZIP is not a regular file"))
	}
	tail := make([]byte, min(info.Size(), int64(65557)))
	if _, err = f.ReadAt(tail, info.Size()-int64(len(tail))); err != nil {
		return fail(err)
	}
	found := false
	for i := len(tail) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) != 0x06054b50 || i+22+int(binary.LittleEndian.Uint16(tail[i+20:])) > len(tail) {
			continue
		}
		size := uint64(binary.LittleEndian.Uint32(tail[i+12:]))
		count := uint64(binary.LittleEndian.Uint16(tail[i+10:]))
		directoryEnd := info.Size() - int64(len(tail)) + int64(i)
		offset := uint64(binary.LittleEndian.Uint32(tail[i+16:]))
		if count == 0xffff || size == 0xffff || size == 0xffffffff || offset == 0xffffffff {
			locator := make([]byte, 20)
			// A legacy ZIP may have exactly 65535 entries without ZIP64.
			if _, e := f.ReadAt(locator, directoryEnd-20); e == nil && binary.LittleEndian.Uint32(locator) == 0x07064b50 {
				record := make([]byte, 56)
				off := binary.LittleEndian.Uint64(locator[8:])
				if off > uint64(info.Size()) || off > 1<<63-1 {
					return fail(fmt.Errorf("invalid ZIP64 offset"))
				}
				if _, e := f.ReadAt(record, int64(off)); e != nil || binary.LittleEndian.Uint32(record) != 0x06064b50 {
					return fail(fmt.Errorf("invalid ZIP64 directory"))
				}
				size = binary.LittleEndian.Uint64(record[40:])
				count = binary.LittleEndian.Uint64(record[32:])
				offset = binary.LittleEndian.Uint64(record[48:])
				directoryEnd = int64(off)
			}
		}
		if size > 32*1024*1024 || count > 100000 {
			return fail(fmt.Errorf("ZIP index exceeds preview budget (32 MiB / 100000 entries); download the original archive"))
		}
		// archive/zip tolerates inaccurate directory sizes and counts, and can
		// scan beyond the declared region. Validate the actual records before
		// it allocates File objects, including its base-offset compatibility.
		start := directoryEnd - int64(size)
		if e := validateZIPDirectory(f, start, info.Size()); e != nil {
			return fail(e)
		}
		if offset < uint64(info.Size()) && int64(offset) != start {
			if e := validateZIPDirectory(f, int64(offset), info.Size()); e != nil {
				return fail(e)
			}
		}
		found = true
		break
	}
	if !found {
		return fail(fmt.Errorf("invalid ZIP central directory"))
	}
	z, err := zip.NewReader(f, info.Size())
	if err != nil && err != zip.ErrInsecurePath {
		return fail(err)
	}
	if len(z.File) > 100000 {
		return fail(fmt.Errorf("ZIP index exceeds preview budget"))
	}
	return z, f, nil
}

func validateZIPDirectory(f *os.File, start, fileSize int64) error {
	if start < 0 || start > fileSize {
		return fmt.Errorf("invalid ZIP directory offset")
	}
	var header [46]byte
	position := start
	for count := 0; ; count++ {
		if _, e := f.ReadAt(header[:], position); e != nil || binary.LittleEndian.Uint32(header[:]) != 0x02014b50 {
			return nil // archive/zip handles malformed records and count mismatch.
		}
		size := int64(46) + int64(binary.LittleEndian.Uint16(header[28:])) + int64(binary.LittleEndian.Uint16(header[30:])) + int64(binary.LittleEndian.Uint16(header[32:]))
		position += size
		if count >= 100000 || position-start > 32*1024*1024 {
			return fmt.Errorf("ZIP index exceeds preview budget (32 MiB / 100000 entries); download the original archive")
		}
		if position > fileSize {
			return fmt.Errorf("invalid ZIP directory size")
		}
	}
}

func archiveSelect(z *zip.Reader, name string, id *int) (*zip.File, int, error) {
	if !safeArchivePath(name) {
		return nil, 0, fmt.Errorf("invalid archive path")
	}
	if id != nil {
		if *id < 0 || *id >= len(z.File) {
			return nil, 0, fmt.Errorf("invalid ZIP entry ID")
		}
		f := z.File[*id]
		if !safeArchivePath(f.Name) || f.Mode()&os.ModeSymlink != 0 || strings.TrimSuffix(f.Name, "/") != strings.TrimSuffix(name, "/") {
			return nil, 0, fmt.Errorf("ZIP entry ID/name mismatch")
		}
		return f, *id, nil
	}
	var selected *zip.File
	index := 0
	for i, f := range z.File {
		if strings.TrimSuffix(f.Name, "/") == strings.TrimSuffix(name, "/") {
			if selected != nil {
				return nil, 0, fmt.Errorf("duplicate ZIP name; select an explicit entry ID")
			}
			selected = f
			index = i
		}
	}
	if selected == nil {
		return nil, 0, os.ErrNotExist
	}
	if selected.Mode()&os.ModeSymlink != 0 {
		return nil, 0, fmt.Errorf("ZIP symlinks are not followed")
	}
	return selected, index, nil
}
func zipView(target string, f *zip.File, id int) map[string]any {
	kind := "file"
	if f.FileInfo().IsDir() {
		kind = "directory"
	}
	return map[string]any{"path": target, "name": path.Base(strings.TrimSuffix(f.Name, "/")), "archive": true, "archiveEntry": strings.TrimSuffix(f.Name, "/"), "entryId": id, "type": kind, "size": f.UncompressedSize64, "modifiedAt": f.Modified}
}
func (runtime *capabilityRuntime) archiveFile(target string, p fileParams) (any, error) {
	if p.Action != "stat" && p.Action != "list" && p.Action != "read" {
		return nil, fmt.Errorf("archive previews are read-only")
	}
	if !safeArchivePath(p.ArchiveEntry) {
		return nil, fmt.Errorf("invalid archive path")
	}
	z, f, err := openPreviewZIP(target)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	archiveInfo, e := f.Stat()
	if e != nil {
		return nil, e
	}
	version := fmt.Sprintf("%x-%x", archiveInfo.Size(), archiveInfo.ModTime().UnixNano())
	if p.ArchiveVersion != "" && p.ArchiveVersion != version {
		return nil, fmt.Errorf("ZIP 已发生变化，请刷新文件树并重新选择条目")
	}
	if p.Action == "stat" && p.ArchiveEntry == "" && p.EntryID == nil {
		return map[string]any{"path": target, "archive": true, "archiveEntry": "", "type": "directory", "archiveVersion": version}, nil
	}
	if p.Action == "list" {
		if p.Cursor < 0 {
			return nil, fmt.Errorf("invalid cursor")
		}
		limit := p.PageSize
		if limit == 0 {
			limit = 250
		}
		if limit < 1 || limit > 1000 {
			return nil, fmt.Errorf("invalid page size")
		}
		prefix := strings.TrimSuffix(p.ArchiveEntry, "/")
		if prefix != "" {
			prefix += "/"
		}
		seen := map[string]bool{}
		entries := []map[string]any{}
		ordinal := 0
		more := false
		for i, entry := range z.File {
			if !safeArchivePath(entry.Name) || entry.Mode()&os.ModeSymlink != 0 || !strings.HasPrefix(entry.Name, prefix) {
				continue
			}
			suffix := strings.TrimPrefix(entry.Name, prefix)
			if suffix == "" {
				continue
			}
			first, rest, dir := strings.Cut(suffix, "/")
			name := prefix + first
			if dir {
				if seen[name] {
					continue
				}
				seen[name] = true
			}
			if ordinal < p.Cursor {
				ordinal++
				continue
			}
			if len(entries) >= limit {
				more = true
				break
			}
			ordinal++
			if dir && rest != "" {
				entries = append(entries, map[string]any{"path": target, "name": first, "archive": true, "archiveEntry": name, "type": "directory"})
			} else {
				entries = append(entries, zipView(target, entry, i))
			}
		}
		for _, item := range entries {
			item["archiveVersion"] = version
		}
		info, e := f.Stat()
		if e != nil {
			return nil, e
		}
		return map[string]any{"path": target, "entries": entries, "nextCursor": ordinal, "hasMore": more, "modifiedAt": info.ModTime(), "archiveEntryCount": len(z.File), "archiveVersion": version}, nil
	}
	entry, id, err := archiveSelect(z, p.ArchiveEntry, p.EntryID)
	if err != nil {
		if p.Action == "stat" && p.EntryID == nil {
			prefix := strings.TrimSuffix(p.ArchiveEntry, "/") + "/"
			if p.ArchiveEntry == "" {
				prefix = ""
			}
			for _, e := range z.File {
				if safeArchivePath(e.Name) && strings.HasPrefix(e.Name, prefix) {
					return map[string]any{"path": target, "archive": true, "archiveEntry": p.ArchiveEntry, "type": "directory", "archiveVersion": version}, nil
				}
			}
		}
		return nil, err
	}
	if p.Action == "stat" {
		item := zipView(target, entry, id)
		item["archiveVersion"] = version
		return item, nil
	}
	if p.Offset < 0 {
		return nil, fmt.Errorf("invalid offset")
	}
	length := int64(maxFileBytes)
	if p.Length != nil {
		length = *p.Length
	}
	if length < 0 || length > maxFileBytes {
		return nil, fmt.Errorf("read exceeds 4 MiB")
	}
	ctx := p.operationContext
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if entry.Method != zip.Store && entry.UncompressedSize64 > uint64(zipPreviewBudget) {
		return nil, fmt.Errorf("selected ZIP entry exceeds 1 GiB preview preparation budget; download the original archive")
	}
	r, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var reader io.Reader = io.LimitReader(cancelReader{ctx, r}, int64(min(entry.UncompressedSize64, uint64(1<<63-2)))+1)
	if entry.Method == zip.Store {
		offset, e := entry.DataOffset()
		if e != nil || offset < 0 || entry.UncompressedSize64 > uint64(archiveInfo.Size()) || offset > archiveInfo.Size()-int64(entry.UncompressedSize64) {
			return nil, fmt.Errorf("invalid ZIP entry bounds")
		}
		section := io.NewSectionReader(f, offset, int64(entry.UncompressedSize64))
		if _, e = section.Seek(p.Offset, io.SeekStart); e != nil {
			return nil, e
		}
		reader = cancelReader{ctx, section}
	} else if _, err = io.CopyN(io.Discard, reader, p.Offset); err != nil && err != io.EOF {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(reader, length))
	if err != nil {
		return nil, err
	}
	content := string(data)
	if p.Encoding == "base64" {
		content = base64.StdEncoding.EncodeToString(data)
	}
	return map[string]any{"content": content, "encoding": p.Encoding, "eof": uint64(p.Offset+int64(len(data))) >= entry.UncompressedSize64}, nil
}
