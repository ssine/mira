package supervisor

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const maxReleaseArchiveBytes int64 = 512 * 1024 * 1024

type GitHubStager struct {
	Repository string
	Client     *http.Client
}

func (stager GitHubStager) Stage(ctx context.Context, version, destination string) (string, error) {
	entries, err := os.ReadDir(destination)
	if err != nil {
		return "", err
	}
	if len(entries) != 0 {
		return "", fmt.Errorf("candidate directory is not empty")
	}
	repository := stager.Repository
	if repository == "" {
		repository = "https://github.com/ssine/mira"
	}
	client := stager.Client
	if client == nil {
		client = http.DefaultClient
	}
	platform, architecture := runtime.GOOS, runtime.GOARCH
	if platform != "linux" && platform != "windows" {
		return "", fmt.Errorf("automatic Mira updates are not available on %s", platform)
	}
	if architecture != "amd64" && architecture != "arm64" {
		return "", fmt.Errorf("automatic Mira updates are not available on %s", architecture)
	}
	extension := "tar.gz"
	if platform == "windows" {
		extension = "zip"
	}
	archiveName := fmt.Sprintf("mira_%s_%s_%s.%s", version, platform, architecture, extension)
	base := strings.TrimRight(repository, "/") + "/releases/download/v" + version + "/"
	checksums, err := downloadLimited(ctx, client, base+"SHA256SUMS", 4*1024*1024)
	if err != nil {
		return "", fmt.Errorf("download release checksums: %w", err)
	}
	expected, err := checksumFor(checksums, archiveName)
	if err != nil {
		return "", err
	}
	archive, err := downloadLimited(ctx, client, base+archiveName, maxReleaseArchiveBytes)
	if err != nil {
		return "", fmt.Errorf("download release archive: %w", err)
	}
	digest := sha256.Sum256(archive)
	if hex.EncodeToString(digest[:]) != expected {
		return "", fmt.Errorf("release archive checksum mismatch")
	}
	if platform == "windows" {
		err = extractZip(archive, destination)
	} else {
		err = extractTarGzip(archive, destination)
	}
	if err != nil {
		return "", err
	}
	if platform == "windows" {
		executable, err := ensureWindowsReleaseAliases(destination)
		if err != nil {
			return "", err
		}
		return executable, nil
	}
	for _, name := range []string{"mira", "mira.exe", "mira-node", "mira-node.exe"} {
		candidate := filepath.Join(destination, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("release archive contains no Mira executable")
}

func ensureWindowsReleaseAliases(directory string) (string, error) {
	image := filepath.Join(directory, "mira-node.exe")
	imageInfo, err := os.Stat(image)
	if err != nil {
		return "", fmt.Errorf("Windows release contains no mira-node.exe: %w", err)
	}
	if !imageInfo.Mode().IsRegular() {
		return "", fmt.Errorf("Windows Mira image is not a regular file")
	}
	roles := []string{
		"mira", "ssh", "sshd", "sshd-session", "sshd-auth", "scp", "sftp", "sftp-server", "ssh-keygen",
		"ssh-shellhost", "ssh-agent", "ssh-add", "ssh-keyscan", "ssh-sk-helper", "ssh-pkcs11-helper",
	}
	for _, role := range roles {
		alias := filepath.Join(directory, role+".exe")
		if info, statErr := os.Stat(alias); statErr == nil {
			if !os.SameFile(imageInfo, info) {
				return "", fmt.Errorf("Windows Mira role %s does not reference the release image", role)
			}
			continue
		} else if !os.IsNotExist(statErr) {
			return "", statErr
		}
		if err := os.Link(image, alias); err != nil {
			return "", fmt.Errorf("create Windows Mira %s role: %w", role, err)
		}
	}
	return filepath.Join(directory, "mira.exe"), nil
}

func downloadLimited(ctx context.Context, client *http.Client, location string, maximum int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "mira-supervisor")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > maximum {
		return nil, fmt.Errorf("download exceeds %d bytes", maximum)
	}
	return payload, nil
}

func checksumFor(payload []byte, name string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(payload)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			if len(fields[0]) != 64 {
				break
			}
			if _, err := hex.DecodeString(fields[0]); err == nil {
				return strings.ToLower(fields[0]), nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("release checksum does not contain %s", name)
}

func safeArchivePath(destination, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe release path %q", name)
	}
	// Strip the release archive's single top-level package directory.
	parts := strings.Split(filepath.ToSlash(clean), "/")
	if len(parts) < 2 {
		return "", nil
	}
	relative := filepath.FromSlash(strings.Join(parts[1:], "/"))
	if relative == "" || relative == "." {
		return "", nil
	}
	target := filepath.Join(destination, relative)
	if !pathInside(destination, target) {
		return "", fmt.Errorf("unsafe release path %q", name)
	}
	return target, nil
}

func extractTarGzip(payload []byte, destination string) error {
	reader, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer reader.Close()
	archive := tar.NewReader(reader)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target, err := safeArchivePath(destination, header.Name)
		if err != nil {
			return err
		}
		if target == "" {
			continue
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if header.Size < 0 || header.Size > maxReleaseArchiveBytes {
				return fmt.Errorf("invalid release member size")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(header.Mode)&0777)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, archive, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(header.Linkname) || strings.Contains(filepath.ToSlash(header.Linkname), "../") {
				return fmt.Errorf("unsafe release symlink")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported release member type")
		}
	}
	return nil
}

func extractZip(payload []byte, destination string) error {
	reader, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		return err
	}
	for _, member := range reader.File {
		target, err := safeArchivePath(destination, member.Name)
		if err != nil {
			return err
		}
		if target == "" {
			continue
		}
		if member.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
			continue
		}
		if member.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Windows release must not contain symlinks")
		}
		if member.UncompressedSize64 > uint64(maxReleaseArchiveBytes) {
			return fmt.Errorf("release member is too large")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		input, err := member.Open()
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, member.Mode()&0777)
		if err != nil {
			input.Close()
			return err
		}
		_, copyErr := io.Copy(output, io.LimitReader(input, maxReleaseArchiveBytes+1))
		input.Close()
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
