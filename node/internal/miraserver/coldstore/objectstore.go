package coldstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"syscall"
	"time"
)

// ObjectConfig describes one S3-compatible bucket. Aliyun OSS, MinIO, Tencent
// COS and other providers accept AWS Signature Version 4 requests.
type ObjectConfig struct {
	Endpoint        string // https://oss-cn-beijing.aliyuncs.com
	Region          string // signing region, for example oss-cn-beijing
	Bucket          string
	Prefix          string // key prefix such as "homeserver/"
	AccessKeyID     string
	SecretAccessKey string
	PathStyle       bool
	// TCPCongestion optionally selects the congestion control algorithm
	// (for example "bbr") for this client's sockets only. Lossy long paths
	// collapse under loss-based algorithms; the host default stays unchanged.
	TCPCongestion string
}

// ErrObjectNotFound reports a missing object without exposing provider details.
var ErrObjectNotFound = errors.New("cold object not found")

// ObjectStore is the minimal object API used by cold history.
type ObjectStore interface {
	Put(ctx context.Context, key string, body []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}

type s3Client struct {
	config   ObjectConfig
	endpoint *url.URL
	http     *http.Client
	now      func() time.Time
}

// maxObjectBytes bounds one downloaded object. Single canonical payloads are
// far below this; a larger response indicates a wrong key or provider fault.
const maxObjectBytes = 256 << 20

func NewS3(config ObjectConfig, client *http.Client) (ObjectStore, error) {
	endpoint, err := url.Parse(strings.TrimRight(config.Endpoint, "/"))
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return nil, fmt.Errorf("cold storage endpoint must be an absolute http(s) URL")
	}
	if config.Bucket == "" || config.Region == "" || config.AccessKeyID == "" || config.SecretAccessKey == "" {
		return nil, fmt.Errorf("cold storage bucket, region and credentials are required")
	}
	if client == nil {
		congestion := config.TCPCongestion
		dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
		if congestion != "" {
			dialer.Control = func(_, _ string, raw syscall.RawConn) error {
				var setErr error
				if err := raw.Control(func(fd uintptr) { setErr = setCongestion(fd, congestion) }); err != nil {
					return err
				}
				return setErr
			}
		}
		// Keep enough idle connections for parallel uploads; each new TLS
		// connection costs several round trips on a long path.
		client = &http.Client{Timeout: 10 * time.Minute, Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, DialContext: dialer.DialContext, ForceAttemptHTTP2: true,
			MaxIdleConns: 128, MaxIdleConnsPerHost: 64, IdleConnTimeout: 90 * time.Second,
			TLSHandshakeTimeout: 20 * time.Second, ExpectContinueTimeout: time.Second,
		}}
	}
	return &s3Client{config: config, endpoint: endpoint, http: client, now: time.Now}, nil
}

func (client *s3Client) objectURL(key string) *url.URL {
	full := client.config.Prefix + key
	result := *client.endpoint
	escaped := s3Escape("/" + full)
	if client.config.PathStyle {
		result.Path = "/" + client.config.Bucket + "/" + full
		result.RawPath = s3Escape("/"+client.config.Bucket) + escaped
	} else {
		result.Host = client.config.Bucket + "." + client.endpoint.Host
		result.Path = "/" + full
		result.RawPath = escaped
	}
	return &result
}

func (client *s3Client) Put(ctx context.Context, key string, body []byte) error {
	digest := md5.Sum(body)
	headers := map[string]string{
		"content-md5":  base64.StdEncoding.EncodeToString(digest[:]),
		"content-type": "application/octet-stream",
	}
	response, err := client.do(ctx, http.MethodPut, key, body, headers)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return providerError("upload", response)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	return nil
}

// providerError keeps the provider's short error code (for example
// AccessDenied or SignatureDoesNotMatch) and discards the rest of the body.
func providerError(operation string, response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	code := ""
	if start := bytes.Index(body, []byte("<Code>")); start >= 0 {
		rest := body[start+6:]
		if end := bytes.Index(rest, []byte("</Code>")); end > 0 && end <= 64 {
			code = " " + string(rest[:end])
		}
	}
	return fmt.Errorf("cold object %s failed: HTTP %d%s", operation, response.StatusCode, code)
}

func (client *s3Client) Get(ctx context.Context, key string) ([]byte, error) {
	response, err := client.do(ctx, http.MethodGet, key, nil, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrObjectNotFound
	}
	if response.StatusCode/100 != 2 {
		return nil, providerError("download", response)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxObjectBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxObjectBytes {
		return nil, fmt.Errorf("cold object exceeds %d bytes", maxObjectBytes)
	}
	return body, nil
}

func (client *s3Client) Delete(ctx context.Context, key string) error {
	response, err := client.do(ctx, http.MethodDelete, key, nil, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 && response.StatusCode != http.StatusNotFound {
		return providerError("delete", response)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	return nil
}

func (client *s3Client) do(ctx context.Context, method, key string, body []byte, extra map[string]string) (*http.Response, error) {
	target := client.objectURL(key)
	request, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body == nil {
		request.Body = http.NoBody
	}
	request.ContentLength = int64(len(body))
	payloadHash := sha256.Sum256(body)
	headers := map[string]string{
		"host":                 target.Host,
		"x-amz-content-sha256": hex.EncodeToString(payloadHash[:]),
		"x-amz-date":           client.now().UTC().Format("20060102T150405Z"),
	}
	for name, value := range extra {
		headers[name] = value
	}
	client.sign(request, target, headers)
	return client.http.Do(request)
}

// sign implements AWS Signature Version 4 for a single-chunk request.
func (client *s3Client) sign(request *http.Request, target *url.URL, headers map[string]string) {
	amzDate := headers["x-amz-date"]
	day := amzDate[:8]
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name + ":" + strings.TrimSpace(headers[name]) + "\n")
		if name != "host" {
			request.Header.Set(name, headers[name])
		}
	}
	signedHeaders := strings.Join(names, ";")
	canonical := strings.Join([]string{
		request.Method, target.EscapedPath(), "", canonicalHeaders.String(), signedHeaders, headers["x-amz-content-sha256"],
	}, "\n")
	scope := day + "/" + client.config.Region + "/s3/aws4_request"
	canonicalHash := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(canonicalHash[:])
	key := hmacSHA256([]byte("AWS4"+client.config.SecretAccessKey), day)
	key = hmacSHA256(key, client.config.Region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, toSign))
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+client.config.AccessKeyID+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)
}

// s3Escape applies the SigV4 canonical URI encoding: every byte except
// unreserved characters and the path separator is percent-encoded.
func s3Escape(path string) string {
	var builder strings.Builder
	for _, value := range []byte(path) {
		switch {
		case value >= 'A' && value <= 'Z', value >= 'a' && value <= 'z', value >= '0' && value <= '9',
			value == '-', value == '_', value == '.', value == '~', value == '/':
			builder.WriteByte(value)
		default:
			fmt.Fprintf(&builder, "%%%02X", value)
		}
	}
	return builder.String()
}

func hmacSHA256(key []byte, value string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(value))
	return mac.Sum(nil)
}
