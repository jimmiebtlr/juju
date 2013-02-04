package api

import (
	"code.google.com/p/go.net/websocket"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"launchpad.net/juju-core/cert"
	"launchpad.net/juju-core/log"
	"launchpad.net/juju-core/trivial"
	"time"
)

type State struct {
	conn *websocket.Conn
}

// Info encapsulates information about a server holding juju state and
// can be used to make a connection to it.
type Info struct {
	// Addrs holds the addresses of the state servers.
	Addrs []string

	// CACert holds the CA certificate that will be used
	// to validate the state server's certificate, in PEM format.
	CACert []byte

	// EntityName holds the name of the entity that is connecting.
	// It should be empty when connecting as an administrator.
	EntityName string

	// Password holds the password for the administrator or connecting entity.
	Password string
}

var openAttempt = trivial.AttemptStrategy{
	Total: 5 * time.Minute,
	Delay: 500 * time.Millisecond,
}

func Open(info *Info) (*State, error) {
	// TODO Select a random address from info.Addrs
	// and only fail when we've tried all the addresses.
	// TODO what does "origin" really mean, and is localhost always ok?
	cfg, err := websocket.NewConfig("wss://"+info.Addrs[0]+"/", "http://localhost/")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	xcert, err := cert.ParseCert(info.CACert)
	if err != nil {
		return nil, err
	}
	pool.AddCert(xcert)
	cfg.TlsConfig = &tls.Config{
		RootCAs:    pool,
		ServerName: "anything",
	}
	var conn *websocket.Conn
	for a := openAttempt.Start(); a.Next(); {
		log.Printf("state/api: dialling %q", cfg.Location)
		conn, err = websocket.DialConfig(cfg)
		if err == nil {
			break
		}
		log.Printf("state/api: %v", err)
	}
	log.Printf("state/api: connection established")
	if err != nil {
		return nil, err
	}
	return &State{
		conn: conn,
	}, nil
}

func (s *State) Close() error {
	return s.conn.Close()
}

// Request is a placeholder for an arbitrary operation in the state API.
// Currently it simply returns the instance id of the machine with the
// id given by the request.
func (s *State) Request(req string) (string, error) {
	err := websocket.JSON.Send(s.conn, rpcRequest{req})
	if err != nil {
		return "", fmt.Errorf("cannot send request: %v", err)
	}
	var resp rpcResponse
	err = websocket.JSON.Receive(s.conn, &resp)
	if err != nil {
		return "", fmt.Errorf("cannot receive response: %v", err)
	}
	if resp.Error != "" {
		return "", errors.New(resp.Error)
	}
	return resp.Response, nil
}