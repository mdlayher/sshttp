package sshttp

import (
	"bytes"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const (
	// sftpNoSuchFile is the error code returned by SFTP if access is attempted
	// to a file which does not exist.
	sftpNoSuchFile = 2
)

// RoundTripper implements http.RoundTripper, and handles performing a HTTP
// request over SSH, using SFTP to send a file in response.  A RoundTripper can
// automatically dial SSH hosts when RoundTrip is called, assuming the correct
// default credentials are provided.  If more control is needed, use the Dial
// method to configure each host on an individual basis.
type RoundTripper struct {
	config *ssh.ClientConfig
	conn   map[string]*clientPair
}

// NewRoundTripper accepts a ssh.ClientConfig struct and returns a
// RoundTripper which can be used by net/http.  The configuration parameter
// is used as the default for any SSH hosts which are not explicitly configured
// using the Dial method.
func NewRoundTripper(config *ssh.ClientConfig) *RoundTripper {
	return &RoundTripper{
		config: config,
		conn:   make(map[string]*clientPair),
	}
}

// Dial attempts to dial a SSH connection to the specified host, using the
// specified SSH client configuration.  If the config parameter is nil,
// the default set by NewRoundTripper will be used.
//
// Dial should be used if more than a single host is being dialed by
// RoundTripper, so that various SSH client configurations may be used, if
// needed.  For a single host, allowing RoundTripper to lazily dial a host
// using the default SSH client configuration is typically acceptable.
func (rt *RoundTripper) Dial(host string, config *ssh.ClientConfig) error {
	// Use default configuration if none specified
	if config == nil {
		config = rt.config
	}

	// Create clientPair with SSH and SFTP clients
	pair, err := dialSSHSFTP(host, config)
	if err != nil {
		return err
	}

	rt.conn[host] = pair
	return nil
}

// Close closes all open SFTP and SSH connections for this RoundTripper.
func (rt *RoundTripper) Close() error {
	// Attempt to close each SFTP and SSH connection.  Map iteration
	// order is undefined in Go, but this is okay for our purposes.
	for k := range rt.conn {
		if err := rt.conn[k].sftpc.Close(); err != nil {
			return err
		}
		if err := rt.conn[k].sshc.Close(); err != nil {
			return err
		}

		delete(rt.conn, k)
	}

	return nil
}

// RoundTrip implements http.RoundTripper, and performs a HTTP request over SSH,
// using SFTP to coordinate the response.  If a SSH connection is not already
// open to the host specified in r.URL.Host, RoundTrip will attempt to lazily
// dial the host using the default configuration from NewRoundTripper.
func (rt *RoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	// Attempt to dial the request host, if needed
	p, err := rt.lazyDial(r.URL.Host)
	if err != nil {
		return nil, err
	}

	switch r.Method {
	// GET - retrieve a file's contents from the remote filesystem
	case "GET":
		return get(p, r)
	}

	// Invalid HTTP method
	return httpResponse(http.StatusMethodNotAllowed, nil, nil), nil
}

// lazyDial attempts to dial a connection to a host if one is not already
// open.  If a connection is open, it returns that connection's clientPair.
func (rt *RoundTripper) lazyDial(host string) (*clientPair, error) {
	// Check for an existing, open connection
	p, ok := rt.conn[host]
	if ok {
		return p, nil
	}

	// Dial a new connection using the default config
	if err := rt.Dial(host, rt.config); err != nil {
		return nil, err
	}

	// Use the new connection for this RoundTrip
	return rt.conn[host], nil
}

// get attempts to retrieve a file from a remote filesystem over SSH, using SFTP
// to return the file's contents in a HTTP response body.
func get(p *clientPair, r *http.Request) (*http.Response, error) {
	// Check for the requested file in the remote filesystem
	f, err := p.sftpc.Open(r.URL.Path)
	if err != nil {
		serr, ok := err.(*sftp.StatusError)
		if !ok {
			return nil, err
		}

		// If file does not exist, send a 404
		if serr.Code == sftpNoSuchFile {
			return httpResponse(http.StatusNotFound, nil, nil), nil
		}

		return nil, err
	}

	// Stat the file to retrieve size and modtime
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}

	// Attach headers for file information
	h := http.Header{}
	h.Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	h.Set("Last-Modified", stat.ModTime().UTC().Format(http.TimeFormat))

	// Attempt to discover Content-Type using file extension
	cType := mime.TypeByExtension(filepath.Ext(stat.Name()))
	if cType != "" {
		h.Set("Content-Type", cType)
	} else {
		// As a fallback, read the first 512 bytes of the file
		// to determine its content type
		buf := bytes.NewBuffer(nil)
		rn, err := io.CopyN(buf, f, 512)
		if err != nil {
			return nil, err
		}
		h.Set("Content-Type", http.DetectContentType(buf.Bytes()[:rn]))

		// Rewind file so the entire file can be transferred
		if _, err := f.Seek(0, os.SEEK_SET); err != nil {
			return nil, err
		}
	}

	// Open an in-memory pipe to stream the file from disk to the HTTP response
	pr, pw := io.Pipe()
	go func() {
		// Transfer file bytes and clean up
		var sErr stickyError
		_, err := io.CopyN(pw, f, stat.Size())
		sErr.Set(err)
		sErr.Set(f.Close())

		// Send any errors during streaming or cleanup to the client
		if err := pw.CloseWithError(sErr.Get()); err != nil {
			panic(err)
		}
	}()

	// Send HTTP response with code, pipe reader body, and headers
	return httpResponse(
		http.StatusOK,
		pr,
		h,
	), nil
}

// httpResponse builds a HTTP response with typical headers using an input
// HTTP status code, response body, and initial HTTP headers.
func httpResponse(code int, body io.ReadCloser, headers http.Header) *http.Response {
	res := &http.Response{
		StatusCode: code,
		ProtoMajor: 1,
		ProtoMinor: 1,

		Body: body,
	}

	// Apply parameter headers and identify server
	h := http.Header{}
	h.Set("Server", "github.com/mdlayher/sshttp")
	for k, v := range headers {
		for _, vv := range v {
			h.Add(k, vv)
		}
	}

	// Apply defaults for headers, if they do not already exist

	const date = "Date"
	if h.Get(date) == "" {
		h.Set(date, time.Now().UTC().Format(http.TimeFormat))
	}

	const contentType = "Content-Type"
	if h.Get(contentType) == "" {
		h.Set(contentType, "text/plain; charset=utf-8")
	}

	const connection = "Connection"
	if code != http.StatusOK && h.Get(connection) == "" {
		h.Set(connection, "close")
	}

	res.Header = h
	return res
}
