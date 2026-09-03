package node

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
)

// SFTP uses the same configured path policy as JSON files, but streams bounded
// packets instead of imposing a total-file-size limit. This is not a sandbox:
// SSH shell/exec intentionally retains the Node OS user's permissions.
type sshFileSystem struct {
	runtime *capabilityRuntime
	handles atomic.Int32
}

func newSSHFileSystem(roots []string) (*sshFileSystem, error) {
	r, err := newCapabilityRuntime(config{AllowedRoots: roots})
	if err != nil {
		return nil, err
	}
	return &sshFileSystem{runtime: r}, nil
}
func (fs *sshFileSystem) wirePath(value string) string {
	value = filepath.ToSlash(value)
	if runtime.GOOS == "windows" && !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	return value
}
func (fs *sshFileSystem) nativePath(value string, allowRoot bool) (string, error) {
	if runtime.GOOS == "windows" {
		// SSH has POSIX paths; /C:/Users maps to C:\Users. Do not permit UNC
		// paths or drive-relative paths, which can trigger network authentication.
		if len(value) < 4 || value[0] != '/' || value[2] != ':' || value[3] != '/' || strings.Contains(value[3:], ":") || strings.Contains(value, "\\") {
			return "", fmt.Errorf("Windows SFTP paths must use /C:/path syntax")
		}
		value = filepath.FromSlash(value[1:])
	}
	return fs.runtime.authorize(value, allowRoot)
}
func (fs *sshFileSystem) RealPath(value string) (string, error) {
	if value == "/" && runtime.GOOS == "windows" {
		return "/", nil
	}
	if !strings.HasPrefix(value, "/") {
		value = path.Join(fs.wirePath(fs.runtime.roots[0]), value)
	}
	_, err := fs.nativePath(value, true)
	if err != nil {
		return "", err
	}
	// Keep the configured namespace, including Windows 8.3 aliases and Unix
	// symlinked roots. Returning a resolved long path could fall outside the
	// lexical configured root on the next request even though it is the same file.
	return path.Clean(value), nil
}

type sshOpenFile struct {
	*os.File
	once    sync.Once
	release func()
}

func (file *sshOpenFile) Close() error {
	var err error
	file.once.Do(func() { err = file.File.Close(); file.release() })
	return err
}
func (fs *sshFileSystem) open(r *sftp.Request, write bool) (*sshOpenFile, error) {
	p, err := fs.nativePath(r.Filepath, !write)
	if err != nil {
		return nil, err
	}
	if fs.handles.Add(1) > 64 {
		fs.handles.Add(-1)
		return nil, fmt.Errorf("SFTP open handle limit reached")
	}
	release := func() { fs.handles.Add(-1) }
	flags := os.O_RDONLY
	if write {
		f := r.Pflags()
		if f.Append {
			release()
			return nil, sftp.ErrSSHFxOpUnsupported
		}
		flags = os.O_WRONLY
		if f.Read {
			flags = os.O_RDWR
		}
		if f.Creat {
			flags |= os.O_CREATE
		}
		if f.Trunc {
			flags |= os.O_TRUNC
		}
		if f.Excl {
			flags |= os.O_EXCL
		}
	}
	// Preflight prevents ordinary FIFO/device opens from blocking a worker.
	if info, statErr := os.Stat(p); statErr == nil && !info.Mode().IsRegular() {
		release()
		return nil, fmt.Errorf("SFTP requires a regular file")
	}
	file, err := os.OpenFile(p, flags, 0600)
	if err != nil {
		release()
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		release()
		return nil, fmt.Errorf("SFTP requires a regular file")
	}
	return &sshOpenFile{File: file, release: release}, nil
}
func (fs *sshFileSystem) Fileread(r *sftp.Request) (io.ReaderAt, error)  { return fs.open(r, false) }
func (fs *sshFileSystem) Filewrite(r *sftp.Request) (io.WriterAt, error) { return fs.open(r, true) }
func (fs *sshFileSystem) OpenFile(r *sftp.Request) (sftp.WriterAtReaderAt, error) {
	return fs.open(r, true)
}
func (fs *sshFileSystem) Filecmd(r *sftp.Request) error {
	p, err := fs.nativePath(r.Filepath, false)
	if err != nil {
		return err
	}
	switch r.Method {
	case "Mkdir":
		return os.Mkdir(p, 0750)
	case "Remove", "Rmdir":
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		if (r.Method == "Rmdir") != info.IsDir() {
			return fmt.Errorf("SFTP remove type mismatch")
		}
		return os.Remove(p)
	case "Rename":
		destination, err := fs.nativePath(r.Target, false)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(destination); !os.IsNotExist(err) {
			return os.ErrExist
		}
		return os.Rename(p, destination)
	case "Setstat":
		flags, attrs := r.AttrFlags(), r.Attributes()
		if flags.UidGid {
			return sftp.ErrSSHFxOpUnsupported
		}
		if flags.Size {
			if attrs.Size > 1<<63-1 {
				return fmt.Errorf("invalid size")
			}
			if err := os.Truncate(p, int64(attrs.Size)); err != nil {
				return err
			}
		}
		if flags.Permissions {
			if err := os.Chmod(p, os.FileMode(attrs.Mode)&0777); err != nil {
				return err
			}
		}
		if flags.Acmodtime {
			return os.Chtimes(p, time.Unix(int64(attrs.Atime), 0), time.Unix(int64(attrs.Mtime), 0))
		}
		return nil
	default:
		return sftp.ErrSSHFxOpUnsupported
	}
}

type sshFileList struct {
	entries []os.FileInfo
	release func()
	once    sync.Once
}

func (list *sshFileList) ListAt(dest []os.FileInfo, offset int64) (int, error) {
	if offset < 0 {
		return 0, fmt.Errorf("negative offset")
	}
	if offset >= int64(len(list.entries)) {
		return 0, io.EOF
	}
	n := copy(dest, list.entries[offset:])
	if n < len(dest) {
		return n, io.EOF
	}
	return n, nil
}
func (list *sshFileList) Close() error { list.once.Do(list.release); return nil }

type sshVolumeInfo struct{ name string }

func (v sshVolumeInfo) Name() string       { return v.name }
func (v sshVolumeInfo) Size() int64        { return 0 }
func (v sshVolumeInfo) Mode() os.FileMode  { return os.ModeDir | 0755 }
func (v sshVolumeInfo) ModTime() time.Time { return time.Time{} }
func (v sshVolumeInfo) IsDir() bool        { return true }
func (v sshVolumeInfo) Sys() any           { return nil }
func (fs *sshFileSystem) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	// The SFTP library closes directory handles, but not the one-shot listers
	// returned by stat. Count only persistent List handles.
	list := &sshFileList{release: func() {}}
	if r.Method == "List" {
		if fs.handles.Add(1) > 64 {
			fs.handles.Add(-1)
			return nil, fmt.Errorf("SFTP open handle limit reached")
		}
		list.release = func() { fs.handles.Add(-1) }
	}
	success := false
	defer func() {
		if !success {
			list.Close()
		}
	}()
	if runtime.GOOS == "windows" && r.Filepath == "/" {
		if r.Method == "Stat" {
			list.entries = []os.FileInfo{sshVolumeInfo{"/"}}
		} else if r.Method == "List" {
			seen := map[string]bool{}
			for _, root := range fs.runtime.roots {
				volume := filepath.VolumeName(root)
				if !seen[volume] {
					list.entries = append(list.entries, sshVolumeInfo{volume})
					seen[volume] = true
				}
			}
		} else {
			return nil, sftp.ErrSSHFxOpUnsupported
		}
		success = true
		return list, nil
	}
	p, err := fs.nativePath(r.Filepath, true)
	if err != nil {
		return nil, err
	}
	switch r.Method {
	case "List":
		file, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		list.entries, err = file.Readdir(10001)
		if err != nil && err != io.EOF {
			return nil, err
		}
		if len(list.entries) > 10000 {
			return nil, fmt.Errorf("directory exceeds 10000 entries")
		}
	case "Stat":
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		list.entries = []os.FileInfo{info}
	default:
		return nil, sftp.ErrSSHFxOpUnsupported
	}
	success = true
	return list, nil
}
