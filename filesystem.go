package sshttp

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// File implements http.File using remote files over SFTP, and is returned
// by FileSystem's Open method.
type File struct {
	// Embed for interface implementation
	*sftp.File

	// Client for use with File.Readdir
	sftpc *sftp.Client

	// Name of file in remote filesystem
	name string

	// Current file offset with File.Readdir
	offset int

	// EOF on next Readdir loop
	eofNext bool
}

// Readdir is used to implement http.File for remote files over SFTP.
// It behaves in the same manner as os.File.Readdir:
// https://godoc.org/os#File.Readdir.
func (f *File) Readdir(count int) ([]os.FileInfo, error) {
	// Return and signal end of files
	if f.eofNext {
		return nil, io.EOF
	}

	// Gather other files in the same directory
	fis, err := f.sftpc.ReadDir(filepath.Dir(f.name))
	if err != nil {
		return nil, err
	}
	sort.Sort(byBaseName(fis))

	// If 0 or negative count is specified, return all files
	// and EOF next.
	if count <= 0 || len(fis) <= count {
		f.eofNext = true
		return fis, nil
	}

	// If files with offset is less than requested length,
	// return the remainder and EOF next.
	if len(fis)-f.offset <= count {
		f.eofNext = true
		return fis[f.offset:], nil
	}

	// If more files exist than requested, return requested
	// number and add to offset
	out := make([]os.FileInfo, count)
	copy(out, fis[f.offset:f.offset+count])
	f.offset += count

	return out, nil
}

// FileSystem implements http.FileSystem for remote files over SFTP.
type FileSystem struct {
	pair *clientPair
	path string
}

// NewFileSystem creates a new FileSystem which can access remote files over
// SFTP.  The resulting FileSystem can be used by net/http to provide access
// to remote files over SFTP, as if they were local.  The host parameter
// specifies the URI to dial and access, and the configuration parameter is
// used to configure the underlying SSH connection.
//
// A host must be a complete URI, including a protocol segment.  For example,
// sftp://127.0.0.1:22/home/foo dials 127.0.0.1 on port 22, and accesses the
// /home/foo directory on the host.
func NewFileSystem(host string, config *ssh.ClientConfig) (*FileSystem, error) {
	// Ensure valid URI with proper protocol
	u, err := url.Parse(host)
	if err != nil {
		return nil, err
	}
	if u.Scheme != Protocol {
		return nil, fmt.Errorf("invalid URL scheme: %s", u.Scheme)
	}

	// Create clientPair with SSH and SFTP clients
	pair, err := dialSSHSFTP(u.Host, config)
	if err != nil {
		return nil, err
	}

	return &FileSystem{
		pair: pair,
		path: u.Path,
	}, nil
}

// Open attempts to access a file under the directory specified in NewFileSystem,
// and attempts to return a http.File for use with net/http.
func (fs *FileSystem) Open(name string) (http.File, error) {
	// Check for the requested file in the remote filesystem
	fpath := filepath.Join(fs.path, name)
	f, err := fs.pair.sftpc.Open(fpath)
	if err != nil {
		return nil, err
	}

	// Create output file
	file := &File{
		File: f,

		sftpc: fs.pair.sftpc,
		name:  fs.path,
	}

	// Check for a directory instead of a file, which requires
	// a slightly different name with a trailing slash
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if stat.IsDir() {
		file.name = fpath + "/"
	}

	return file, nil
}

// Close closes open SFTP and SSH connections for this FileSystem.
func (fs *FileSystem) Close() error {
	var sErr stickyError
	sErr.Set(fs.pair.sftpc.Close())
	sErr.Set(fs.pair.sshc.Close())

	return sErr.Get()
}

// byBaseName implements sort.Interface to sort []os.FileInfo.
type byBaseName []os.FileInfo

func (b byBaseName) Len() int               { return len(b) }
func (b byBaseName) Less(i int, j int) bool { return b[i].Name() < b[j].Name() }
func (b byBaseName) Swap(i int, j int)      { b[i], b[j] = b[j], b[i] }
