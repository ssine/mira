package node

import (
	"archive/zip"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ssine/mira/node/internal/resourcewire"
)

func (client *controlClient) startFileStream(ctx context.Context, message controlMessage) {
	client.fileMu.Lock()
	if client.fileWorkers == nil {
		client.fileWorkers = map[string]context.CancelFunc{}
	}
	if len(client.fileWorkers) >= 32 || client.fileWorkers[message.SessionID] != nil {
		client.fileMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	client.fileWorkers[message.SessionID] = cancel
	client.fileMu.Unlock()
	go func() {
		defer cancel()
		defer func() { client.fileMu.Lock(); delete(client.fileWorkers, message.SessionID); client.fileMu.Unlock() }()
		u, err := url.Parse(client.endpoints.endpoint(ctx))
		if err != nil {
			return
		}
		if u.Scheme == "https" {
			u.Scheme = "wss"
		} else {
			u.Scheme = "ws"
		}
		u.Path = "/v1/file-streams/" + message.SessionID
		u.RawQuery = ""
		u.Fragment = ""
		dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second, ReadBufferSize: resourcewire.FrameBytes, WriteBufferSize: resourcewire.FrameBytes, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: platformCertificatePool()}, Subprotocols: []string{resourcewire.Protocol, "auth." + base64.RawURLEncoding.EncodeToString([]byte(client.token))}}
		ws, _, err := dialer.DialContext(ctx, u.String(), nil)
		if err != nil {
			return
		}
		defer ws.Close()
		ws.SetReadLimit(4096)
		go func() {
			defer cancel()
			for {
				if _, _, e := ws.ReadMessage(); e != nil {
					return
				}
			}
		}()
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				ws.Close()
			case <-done:
			}
		}()
		var p resourcewire.Request
		if json.Unmarshal(message.Params, &p) != nil {
			return
		}
		w := &fileStreamWriter{ws: ws, headers: http.Header{}}
		identity, err := acquireExecutionIdentity(executionRequest{Context: p.ExecutionContext, UserSessionID: p.UserSessionID})
		if err == nil {
			defer identity.close()
			p.CacheIdentity = identity.Context + ":" + identity.OSIdentity
			if identity.UserSessionID != nil {
				p.CacheIdentity += fmt.Sprintf(":%d", *identity.UserSessionID)
			}
			for _, item := range identity.environment {
				key, value, ok := strings.Cut(item, "=")
				if ok && (strings.EqualFold(key, "TEMP") || strings.EqualFold(key, "TMPDIR")) && filepath.IsAbs(value) {
					p.CacheDirectory = value
					break
				}
			}
			_, err = identity.runImpersonated(func() (any, error) { return nil, client.runtime.serveFileContent(ctx, w, p) })
		}
		if err != nil {
			if !w.started {
				status := 400
				if os.IsNotExist(err) {
					status = 404
				}
				if os.IsPermission(err) {
					status = 403
				}
				if p.DiagnosticURL != "" {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.WriteHeader(status)
					_, _ = fmt.Fprintf(w, "<!doctype html><meta charset=\"utf-8\"><title>Mira 网站资源</title><h1>无法读取网站资源</h1><p>%s</p><p>当前根目录：%s</p><a href=\"%s\">调整根目录或入口</a>", html.EscapeString(err.Error()), html.EscapeString(p.Root), html.EscapeString(p.DiagnosticURL))
				} else {
					http.Error(w, err.Error(), status)
				}
			} else {
				ws.Close()
				return
			}
		}
		if !w.started {
			w.WriteHeader(200)
		}
		_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"done":true}`))
	}()
}
func (client *controlClient) stopFileStream(id string) {
	client.fileMu.Lock()
	cancel := client.fileWorkers[id]
	client.fileMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

type fileStreamWriter struct {
	ws      *websocket.Conn
	headers http.Header
	started bool
}

func (w *fileStreamWriter) Header() http.Header { return w.headers }
func (w *fileStreamWriter) WriteHeader(status int) {
	if w.started {
		return
	}
	w.started = true
	w.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_ = w.ws.WriteJSON(resourcewire.Response{Status: status, Headers: w.headers})
}
func (w *fileStreamWriter) Write(data []byte) (int, error) {
	if !w.started {
		w.WriteHeader(200)
	}
	total := 0
	for len(data) > 0 {
		n := min(len(data), resourcewire.FrameBytes)
		w.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if err := w.ws.WriteMessage(websocket.BinaryMessage, data[:n]); err != nil {
			return total, err
		}
		data = data[n:]
		total += n
	}
	return total, nil
}

type cancelReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r cancelReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(b)
}
func (runtime *capabilityRuntime) serveFileContent(ctx context.Context, w http.ResponseWriter, p resourcewire.Request) error {
	if p.Method != "GET" && p.Method != "HEAD" {
		return fmt.Errorf("read-only method required")
	}
	target, err := runtime.authorize(p.Path, true)
	if err != nil {
		return err
	}
	if p.Root != "" && !p.Archive {
		root, e := runtime.authorize(p.Root, true)
		if e != nil {
			return e
		}
		if !pathContained(root, target) {
			return fmt.Errorf("resource resolves outside preview root")
		}
	}
	var content io.ReadSeeker
	var info os.FileInfo
	name := filepath.Base(target)
	file, err := os.Open(target)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("resource is not a regular file")
	}
	content = file
	// The same descriptor is used for metadata and bytes throughout this response.
	version := fmt.Sprintf(`"%x-%x"`, info.Size(), info.ModTime().UnixNano())
	if p.Archive {
		file.Close()
		z, archive, e := openPreviewZIP(target)
		if e != nil {
			return e
		}
		defer archive.Close()
		info, e = archive.Stat()
		if e != nil {
			return e
		}
		if p.ArchiveVersion != "" && p.ArchiveVersion != fmt.Sprintf("%x-%x", info.Size(), info.ModTime().UnixNano()) {
			return fmt.Errorf("ZIP 已发生变化，请刷新文件树并重新选择条目")
		}
		entry, id, e := archiveSelect(z, p.ArchiveEntry, p.EntryID)
		if e != nil {
			return e
		}
		if entry.FileInfo().IsDir() || entry.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("entry is not a regular file")
		}
		if p.Root != "" && p.ArchiveEntry != p.Root && !strings.HasPrefix(p.ArchiveEntry, strings.TrimSuffix(p.Root, "/")+"/") {
			return fmt.Errorf("entry outside preview root")
		}
		name = filepath.Base(entry.Name)
		version = fmt.Sprintf(`"%x-%x-%x-%x"`, info.Size(), info.ModTime().UnixNano(), id, entry.CRC32)
		if entry.Method == zip.Store {
			offset, e := entry.DataOffset()
			if e != nil {
				return e
			}
			if entry.UncompressedSize64 > uint64(info.Size()) || offset < 0 || offset+int64(entry.UncompressedSize64) > info.Size() {
				return fmt.Errorf("invalid ZIP entry bounds")
			}
			content = io.NewSectionReader(archive, offset, int64(entry.UncompressedSize64))
		} else {
			key := target + "\x00" + version + "\x00" + p.CacheIdentity
			temp, release, e := runtime.openCachedZIPEntry(ctx, key, entry, p.CacheDirectory)
			if e != nil {
				return e
			}
			defer release()

			content = temp
		}
	}
	w.Header().Set("ETag", version)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if typ := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); typ != "" {
		w.Header().Set("Content-Type", typ)
	}
	req := (&http.Request{Method: p.Method, Header: p.Headers, URL: &url.URL{Path: "/" + name}}).WithContext(ctx)
	// ServeContent handles suffix, multi-range, If-Range and conditional reads.
	// Each content reader is local; no seek is simulated over sequential deflate.
	http.ServeContent(w, req, name, info.ModTime(), content)
	if !p.Archive {
		after, e := file.Stat()
		if e != nil || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
			return fmt.Errorf("file changed during transfer")
		}
	}
	return nil
}
