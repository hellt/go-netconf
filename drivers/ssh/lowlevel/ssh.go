// Go NETCONF Client
//
// Copyright (c) 2013-2018, Juniper Networks, Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netconf

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"strings"
	"time"

	session "github.com/arsonistgopher/go-netconf/session"
	transport "github.com/arsonistgopher/go-netconf/transport"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

const (
	// DefaultPort is the default SSH port used when communicating with
	// NETCONF
	DefaultPort = 830
	// sshNetconfSubsystem sets the SSH subsystem to NETCONF
	sshNetconfSubsystem = "netconf"
)

// TransportSSH maintains the information necessary to communicate with the
// remote device over SSH
type TransportSSH struct {
	transport.TransportBasicIO              // Embedded Transport basic IO base type
	SSHClient                  *ssh.Client  // SSH Client
	SSHSession                 *ssh.Session // SSH Client Session
}

// Close closes an existing SSH session and socket if they exist.
func (t *TransportSSH) Close() error {
	// Close the SSH Session if we have one
	if t.SSHSession != nil {
		if err := t.SSHSession.Close(); err != nil {
			return err
		}
	}

	// Close the socket
	return t.SSHClient.Close()
}

// DialSSH connects and establishes SSH sessions
//
// target can be an IP address (e.g.) 172.16.1.1 which utlizes the default
// NETCONF over SSH port of 830.  Target can also specify a port with the
// following format <host>:<port (e.g 172.16.1.1:22)
//
// config takes a ssh.ClientConfig connection. See documentation for
// go.crypto/ssh for documentation.  There is a helper function SSHConfigPassword
// thar returns a ssh.ClientConfig for simple username/password authentication
func (t *TransportSSH) DialSSH(target string, config *ssh.ClientConfig, port int) error {
	if !strings.Contains(target, ":") {
		sshport := 0
		if port != 0 {
			sshport = port
		} else {
			sshport = DefaultPort
		}
		target = fmt.Sprintf("%s:%d", target, sshport)
	}

	var err error

	t.SSHClient, err = ssh.Dial("tcp", target, config)
	if err != nil {
		return err
	}

	err = t.SetupSession()
	if err != nil {
		return err
	}

	return nil
}

// SetupSession sorts out wiring
func (t *TransportSSH) SetupSession() error {
	var err error

	t.SSHSession, err = t.SSHClient.NewSession()
	if err != nil {
		return err
	}

	writer, err := t.SSHSession.StdinPipe()
	if err != nil {
		return err
	}

	reader, err := t.SSHSession.StdoutPipe()
	if err != nil {
		return err
	}

	t.ReadWriteCloser = transport.NewReadWriteCloser(reader, writer)
	return t.SSHSession.RequestSubsystem(sshNetconfSubsystem)
}

// NewSSHSession creates a new NETCONF session using an existing net.Conn.
func NewSSHSession(conn net.Conn, config *ssh.ClientConfig) (*session.Session, error) {
	t, err := connToTransport(conn, config)
	if err != nil {
		return nil, err
	}

	return session.NewSession(t), nil
}

// Dial creates a new NETCONF session using a SSH Transport.
// See TransportSSH.Dial for arguments.
func Dial(target string, config *ssh.ClientConfig, port int) (*session.Session, error) {
	var t TransportSSH
	err := t.DialSSH(target, config, port)
	if err != nil {
		return nil, err
	}
	return session.NewSession(&t), nil
}

// DialSSHTimeout creates a new NETCONF session using a SSH Transport with timeout.
// See TransportSSH.Dial for arguments.
// The timeout value is used for both connection establishment and Read/Write operations.
func DialSSHTimeout(target string, config *ssh.ClientConfig, timeout time.Duration) (*session.Session, error) {
	bareConn, err := net.DialTimeout("tcp", target, timeout)
	if err != nil {
		return nil, err
	}

	conn := &deadlineConn{Conn: bareConn, timeout: timeout}
	t, err := connToTransport(conn, config)
	if err != nil {
		return nil, err
	}

	go func() {
		ticker := time.NewTicker(timeout / 2)
		defer ticker.Stop()
		for range ticker.C {
			_, _, err := t.SSHClient.Conn.SendRequest("KEEP_ALIVE", true, nil)
			if err != nil {
				return
			}
		}
	}()

	return session.NewSession(t), nil
}

// SSHConfigPassword is a convenience function that takes a username and password
// and returns a new ssh.ClientConfig setup to pass that username and password.
// Convenience means that HostKey checks are disabled so it's probably less secure
func SSHConfigPassword(user string, pass string) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.Password(pass),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
}

// SSHConfigPubKeyFile is a convenience function that takes a username, private key
// and passphrase and returns a new ssh.ClientConfig setup to pass credentials
// to DialSSH
func SSHConfigPubKeyFile(user string, file string, passphrase string) (*ssh.ClientConfig, error) {
	buf, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(buf)
	if len(rest) > 0 {
		return nil, fmt.Errorf("pem: unable to decode file %s", file)
	}

	if x509.IsEncryptedPEMBlock(block) {
		b, err := x509.DecryptPEMBlock(block, []byte(passphrase))
		if err != nil {
			return nil, err
		}
		buf = pem.EncodeToMemory(&pem.Block{
			Type:  block.Type,
			Bytes: b,
		})
	}

	key, err := ssh.ParsePrivateKey(buf)
	if err != nil {
		return nil, err
	}
	return &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(key),
		},
	}, nil

}

// SSHConfigPubKeyAgent is a convience function that takes a username and
// returns a new ssh.Clientconfig setup to pass credentials received from
// an ssh agent
func SSHConfigPubKeyAgent(user string) (*ssh.ClientConfig, error) {
	c, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK"))
	if err != nil {
		return nil, err
	}
	return &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeysCallback(agent.NewClient(c).Signers),
		},
	}, nil
}

func connToTransport(conn net.Conn, config *ssh.ClientConfig) (*TransportSSH, error) {
	c, chans, reqs, err := ssh.NewClientConn(conn, conn.RemoteAddr().String(), config)
	if err != nil {
		return nil, err
	}

	t := &TransportSSH{}
	t.SSHClient = ssh.NewClient(c, chans, reqs)

	err = t.SetupSession()
	if err != nil {
		return nil, err
	}

	return t, nil
}

type deadlineConn struct {
	net.Conn
	timeout time.Duration
}

func (c *deadlineConn) Read(b []byte) (n int, err error) {
	c.SetReadDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(b)
}

func (c *deadlineConn) Write(b []byte) (n int, err error) {
	c.SetWriteDeadline(time.Now().Add(c.timeout))
	return c.Conn.Write(b)
}
