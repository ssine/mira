package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Installer logic evolves with the archive layout. Always use the selected
// release's installer, not the older script saved when this Node was installed.
func downloadUpdateInstaller(ctx context.Context, baseURL, directory, name string) (string, error) {
	if name != "install.sh" && name != "install.ps1" {
		return "", fmt.Errorf("invalid release installer")
	}
	client := &http.Client{Timeout: 45 * time.Second}
	read := func(asset string) ([]byte, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/"+asset, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("User-Agent", "mira/"+Version)
		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("download %s: HTTP %d", asset, response.StatusCode)
		}
		content, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
		if err != nil {
			return nil, err
		}
		if len(content) > 1024*1024 {
			return nil, fmt.Errorf("release asset %s exceeds size limit", asset)
		}
		return content, nil
	}
	checksums, err := read("SHA256SUMS")
	if err != nil {
		return "", err
	}
	var expected string
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			if expected != "" {
				return "", fmt.Errorf("duplicate installer checksum")
			}
			digest, err := hex.DecodeString(fields[0])
			if err != nil || len(digest) != sha256.Size {
				return "", fmt.Errorf("invalid installer checksum")
			}
			expected = strings.ToLower(fields[0])
		}
	}
	if expected == "" {
		return "", fmt.Errorf("missing checksum for %s", name)
	}
	content, err := read(name)
	if err != nil {
		return "", err
	}
	actual := sha256.Sum256(content)
	if hex.EncodeToString(actual[:]) != expected {
		return "", fmt.Errorf("installer checksum verification failed")
	}
	file, err := os.CreateTemp(directory, ".update-*-"+name)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		file.Close()
		if !ok {
			os.Remove(file.Name())
		}
	}()
	if err := protectIdentityFile(file); err != nil {
		return "", err
	}
	if _, err := file.Write(content); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	ok = true
	return file.Name(), nil
}
