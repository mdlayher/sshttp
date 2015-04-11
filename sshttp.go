// Package sshttp provides functionality that enables some functionality of Go's
// net/http package to be used with SSH servers using SFTP.  MIT Licensed.
package sshttp

import (
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const (
	// Protocol is the protocol which identifies SFTP as the proper scheme for
	// a URL used by this package.
	Protocol = "sftp"
)

// clientPair stores a pair of SSH and SFTP client structs which are connected
// to a single host.
type clientPair struct {
	sshc  *ssh.Client
	sftpc *sftp.Client
}

// dialSSHSFTP dials a SSH connection to the specified host using the specified
// configuration, and then creates a SFTP client using the underlying SSH
// connection.  Both are returned in a clientPair struct, which is used by various
// types in this package.
func dialSSHSFTP(host string, config *ssh.ClientConfig) (*clientPair, error) {
	// Open initial SSH connection
	sshc, err := ssh.Dial("tcp", host, config)
	if err != nil {
		return nil, err
	}

	// Open SFTP subsystem using SSH connection
	sftpc, err := sftp.NewClient(sshc)
	if err != nil {
		return nil, err
	}

	return &clientPair{
		sshc:  sshc,
		sftpc: sftpc,
	}, nil
}

// stickyError is an error which traps the first error sent to Set, and
// returns it once Get is called.  It will ignore any subsequent errors.
type stickyError struct {
	err error
}

// Error implements the error interface for stickyError.
func (e *stickyError) Error() string {
	return e.err.Error()
}

// Set accepts an input error.  If an error is already occurred, it is
// ignored.  If one has not yet occurred, it is stored.
func (e *stickyError) Set(err error) {
	if e.err != nil {
		return
	}

	e.err = err
}

// Get returns the first error stickyError received.
func (e *stickyError) Get() error {
	return e.err
}
