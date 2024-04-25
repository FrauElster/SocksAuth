package socksauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"syscall"
)

const (
	_SOCKS_VERSION         = 0x05
	_NO_ACCEPTABLE_METHODS = 0xff

	_NO_AUTHENTICATION      = 0x00
	_USERNAME_PASSWORD_AUTH = 0x02
	_CONNECT                = 0x01

	_STATUS_OK                   = 0x00
	_GENERAL_SOCKS_FAILURE       = 0x01
	_CONN_NOT_ALLOWED_BY_RULESET = 0x02
	_NETWORK_UNREACHABLE         = 0x03
	_HOST_UNREACHABLE            = 0x04
	_CONN_REFUSED                = 0x05
	_TTL_EXPIRED                 = 0x06
	_COMMAND_NOT_SUPPORTED       = 0x07
	_ADDRESS_TYPE_NOT_SUPPORTED  = 0x08

	_IP_V4       = 0x01
	_DOMAIN_NAME = 0x03
	_IP_V6       = 0x04
)

type Server struct {
	Addr string

	RemoteHost string
	RemoteUser string
	RemotePass string

	ConnCount     atomic.Int64
	OpenConnCount atomic.Int32
	openConnLimit uint32

	onConnect    func(id int64, conn net.Conn)
	onDisconnect func(id int64, conn net.Conn)
	onError      func(id int64, conn net.Conn, err error)

	serverFinder func(context.Context) (string, error)
}

type ServerOption func(*Server)

// WithOpenConnLimit sets the maximum number of open connections the server will accept
// Default is 0, which means no limit
func WithOpenConnLimit(limit uint32) ServerOption {
	return func(s *Server) { s.openConnLimit = limit }
}

// WithOnConnect sets the onConnect callback which is called when a new connection is accepted
// To not block the server the callback is called in a new goroutine
func WithOnConnect(fn func(id int64, conn net.Conn)) ServerOption {
	return func(s *Server) { s.onConnect = fn }
}

// WithOnDisconnect sets the onDisconnect callback which is called when a connection is closed
// To not block the server the callback is called in a new goroutine
func WithOnDisconnect(fn func(id int64, conn net.Conn)) ServerOption {
	return func(s *Server) { s.onDisconnect = fn }
}

// WithOnError sets the onError callback which is called when an error occurs on a connection.
// If the error occurs while accepting a connection the id is 0 and the conn argument will be nil
// To not block the server the callback is called in a new goroutine
func WithOnError(fn func(id int64, conn net.Conn, err error)) ServerOption {
	return func(s *Server) { s.onError = fn }
}

// WithServerFinder sets the function to find a SOCKS5 server if no remoteHost is provided
// Default will find a NordVPN server and try to authenticate with it
func WithServerFinder(fn func(context.Context) (string, error)) ServerOption {
	return func(s *Server) { s.serverFinder = fn }
}

// WithAddr sets the address the server will listen on
// Default is ":1080"
func WithAddr(addr string) ServerOption {
	return func(s *Server) { s.Addr = addr }
}

// NewServer creates a new SOCKS5 server
// if the remoteHost is empty the server will try to find a server using the serverFinder function specified in the WithServerFinder option (default is FindNordVpnServer)
// if the remoteUser and remotePass are empty the server will not authenticate with the remote server, so it is just a simple SOCKS5 proxy, no auth.
func NewServer(remoteHost, remoteUser, remotePass string, opts ...ServerOption) *Server {
	s := &Server{
		Addr:       ":1080",
		RemoteHost: remoteHost,
		RemoteUser: remoteUser,
		RemotePass: remotePass,

		ConnCount:     atomic.Int64{},
		OpenConnCount: atomic.Int32{},

		serverFinder: FindNordVpnServer,
	}

	for _, opt := range opts {
		opt(s)
	}

	// we call the serverFinder here to give the injection the chance to cache (the default FindNordVpnServer does)
	if s.RemoteHost == "" {
		s.serverFinder(context.Background())
	}

	return s
}

func (s *Server) Start(ctx context.Context) error {
	l, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	defer l.Close()
	s.Addr = "socks5://" + l.Addr().String()

	var semaphore chan struct{}
	if s.openConnLimit > 0 {
		semaphore = make(chan struct{}, s.openConnLimit)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		conn, err := l.Accept()
		if err != nil {
			if s.onError != nil {
				err = fmt.Errorf("error accepting connection: %w", err)
				go s.onError(0, conn, err)
			}
			continue
		}

		// aquire semaphore
		if semaphore != nil {
			semaphore <- struct{}{}
		}
		go func() {
			connId := s.ConnCount.Add(1)
			s.OpenConnCount.Add(1)
			if s.onConnect != nil {
				go s.onConnect(connId, conn)
			}
			connCtx, cancel := context.WithCancel(ctx)

			s.handleConnection(connCtx, connId, conn)

			if s.onDisconnect != nil {
				go s.onDisconnect(connId, conn)
			}
			conn.Close()
			cancel()
			s.OpenConnCount.Add(-1)
			// release semaphore
			if semaphore != nil {
				semaphore <- struct{}{}
			}
		}()
	}
}

func (s *Server) handleConnection(ctx context.Context, connId int64, clientConn net.Conn) {
	// Greet the client
	if err := greetClient(clientConn); err != nil {
		if s.onError != nil {
			err = fmt.Errorf("error greeting client: %w", err)
			go s.onError(connId, clientConn, err)
		}
		return
	}

	// Connect to the remote SOCKS5 server
	remoteConn, remoteHost, err := s.getTcpConn(ctx)
	if err != nil {
		if s.onError != nil {
			go s.onError(connId, clientConn, err)
		}
		return
	}
	defer remoteConn.Close()

	// Authenticate with the remote SOCKS5 server
	// TODO: implement unauthenticated connection
	err = authenticateRemoteSocks(remoteConn, s.RemoteUser, s.RemotePass)
	if err != nil {
		if s.onError != nil {
			err = fmt.Errorf("error authenticating with remote server (%s | %s): %w", remoteHost, remoteConn.RemoteAddr().String(), err)
			go s.onError(connId, clientConn, err)
		}
		return
	}

	// Forward the client's request to the remote SOCKS5 server
	err = sendRemoteRequest(clientConn, remoteConn)
	if err != nil {
		if s.onError != nil {
			err = fmt.Errorf("error sending request to remote server: %w", err)
			go s.onError(connId, clientConn, err)
		}
		return
	}

	// Relay data between the client and the remote SOCKS5 server
	err = syncConns(clientConn, remoteConn)
	if err != nil {
		if s.onError != nil {
			err = fmt.Errorf("error syncing connections: %w", err)
			go s.onError(connId, clientConn, err)
		}
		return
	}
}

func (s *Server) getTcpConn(ctx context.Context) (conn net.Conn, remoteHost string, err error) {
	remoteHost = s.RemoteHost
	if remoteHost == "" {
		remoteHost, err = s.serverFinder(ctx)
		if err != nil {
			return nil, remoteHost, err
		}
	}
	remoteHost = strings.TrimPrefix(remoteHost, "socks5://")
	if !strings.Contains(remoteHost, ":") {
		remoteHost += ":1080"
	}

	conn, err = net.Dial("tcp", remoteHost)
	if err != nil {
		err = fmt.Errorf("error connecting to remote server (%s): %w", remoteHost, err)
		return nil, remoteHost, err
	}

	return conn, remoteHost, nil
}

func greetClient(clientConn net.Conn) error {
	// https://datatracker.ietf.org/doc/html/rfc1928#section-3
	header := make([]byte, 2)
	if _, err := io.ReadFull(clientConn, header); err != nil {
		return fmt.Errorf("error reading header: %w", err)
	}

	socksVersion := header[0]
	if socksVersion != _SOCKS_VERSION {
		return fmt.Errorf("unsupported SOCKS version: %d", socksVersion)
	}

	numMethods := int(header[1])
	methods := make([]byte, numMethods)
	if _, err := io.ReadFull(clientConn, methods); err != nil {
		return fmt.Errorf("error reading methods: %w", err)
	}

	if !contains(methods, _NO_AUTHENTICATION) {
		clientConn.Write([]byte{_SOCKS_VERSION, _NO_ACCEPTABLE_METHODS})
		return fmt.Errorf("no supported authentication methods")
	}

	clientConn.Write([]byte{_SOCKS_VERSION, _NO_AUTHENTICATION})
	return nil
}

func authenticateRemoteSocks(conn net.Conn, username, password string) error {
	// Send the authentication methods supported by the client https://datatracker.ietf.org/doc/html/rfc1928#section-3
	_, err := conn.Write([]byte{
		_SOCKS_VERSION,
		0x01, // number of auth methods
		_USERNAME_PASSWORD_AUTH,
	})
	if err != nil {
		return fmt.Errorf("error sending authentication methods: %w", err)
	}

	// Read the server's choice of authentication method
	response := make([]byte, 2)
	if _, err := io.ReadFull(conn, response); err != nil {
		return fmt.Errorf("error reading authentication method selection: %w", err)
	}

	// Check if the server selected username/password authentication
	if response[1] != _USERNAME_PASSWORD_AUTH {
		return fmt.Errorf("server did not select username/password authentication, selected method: %d", response[1])
	}

	// Then, send the username and password
	// https://datatracker.ietf.org/doc/html/rfc1929#section-2
	authRequest := make([]byte, 3+len(username)+len(password)) // 1 byte to specify subnegotiation version, 1 byte for username length, 1 byte for password length
	authRequest[0] = 0x01                                      // version 1 of the subnegotiation
	authRequest[1] = byte(len(username))
	copy(authRequest[2:], username)
	authRequest[2+len(username)] = byte(len(password))
	copy(authRequest[3+len(username):], password)

	_, err = conn.Write(authRequest)
	if err != nil {
		return fmt.Errorf("error sending username/password: %w", err)
	}

	// Read the server's response
	response = make([]byte, 2)
	response[1] = 0xff // Set the response to an invalid value to check if the server changes it (00 is success)
	if _, err := io.ReadFull(conn, response); err != nil {
		return fmt.Errorf("error reading authentication response: %w", err)
	}

	// Check the server's response
	if response[1] != _STATUS_OK {
		return fmt.Errorf("authentication failed")
	}

	return nil
}

func syncConns(clientConn, remoteConn net.Conn) error {
	done := make(chan error, 1)

	// Relay data from client to remote
	go func() {
		_, err := io.Copy(remoteConn, clientConn)
		if errors.Is(err, syscall.ECONNRESET) {
			done <- nil // this happens when the client disconnects abruptly, rude but not an error
		}
		if err != nil {
			done <- fmt.Errorf("error copying data from client to remote: %w", err)
			return
		}

		done <- nil
	}()

	// Relay data from remote to client
	go func() {
		_, err := io.Copy(clientConn, remoteConn)
		if errors.Is(err, syscall.ECONNRESET) {
			done <- nil // this happens when the client disconnects abruptly, rude but not an error
		}
		if err != nil {
			done <- fmt.Errorf("error copying data from remote to client: %w", err)
			return
		}

		done <- nil
	}()

	// Wait for either goroutine to finish
	err := <-done
	clientConn.Close()
	remoteConn.Close()

	return err
}

func sendRemoteRequest(clientConn, remoteConn net.Conn) error {
	request, err := readSocks5Request(clientConn)
	if err != nil {
		return fmt.Errorf("error reading request from client: %w", err)
	}

	_, err = remoteConn.Write(request)
	if err != nil {
		return fmt.Errorf("error forwarding request to remote server: %w", err)
	}

	response, err := readSocks5Response(remoteConn)
	if err != nil {
		return fmt.Errorf("error reading response from remote server: %w", err)
	}

	_, err = clientConn.Write(response)
	if err != nil {
		return fmt.Errorf("error forwarding response to client: %w", err)
	}

	return nil
}

func readSocks5Request(conn net.Conn) ([]byte, error) {
	// Read the SOCKS request from the client https://datatracker.ietf.org/doc/html/rfc1928#section-4
	// Read the first 4 Bytes of the request, the fourth byte determines the length of the rest of the request
	requestHeader := make([]byte, 4)
	if _, err := io.ReadFull(conn, requestHeader); err != nil {
		return nil, fmt.Errorf("error reading request header: %w", err)
	}

	version := requestHeader[0]
	if version != _SOCKS_VERSION {
		return nil, fmt.Errorf("unsupported SOCKS version: %d", version)
	}

	cmd := requestHeader[1]
	if cmd != _CONNECT {
		conn.Write([]byte{_SOCKS_VERSION, _COMMAND_NOT_SUPPORTED})
		return nil, fmt.Errorf("unsupported command: %d", cmd)
	}

	// Determine the length of the remaining part of the request based on the address type
	addrLen := 0
	switch requestHeader[3] { // ATYP, the address type
	case _IP_V4:
		addrLen = net.IPv4len
	case _DOMAIN_NAME:
		lengthByte := make([]byte, 1)
		if _, err := io.ReadFull(conn, lengthByte); err != nil {
			return nil, fmt.Errorf("error reading domain name length: %w", err)
		}
		requestHeader = append(requestHeader, lengthByte...)
		addrLen = int(lengthByte[0])
	case _IP_V6:
		addrLen = net.IPv6len
	default:
		conn.Write([]byte{_SOCKS_VERSION, _ADDRESS_TYPE_NOT_SUPPORTED})
		return nil, fmt.Errorf("unknown address type: %d", requestHeader[3])
	}

	// Read the rest of the request
	requestRest := make([]byte, addrLen+2) // +2 for port number
	if _, err := io.ReadFull(conn, requestRest); err != nil {
		conn.Write([]byte{_SOCKS_VERSION, _GENERAL_SOCKS_FAILURE})
		return nil, fmt.Errorf("error reading the rest of the request: %w", err)
	}

	// Combine the header and the rest of the request
	fullRequest := append(requestHeader, requestRest...)
	return fullRequest, nil
}

func readSocks5Response(conn net.Conn) ([]byte, error) {
	// Read the SOCKS response from the remote server
	// https://datatracker.ietf.org/doc/html/rfc1928#section-6
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		conn.Write([]byte{_SOCKS_VERSION, _GENERAL_SOCKS_FAILURE})
		return nil, fmt.Errorf("error reading response header: %w", err)
	}

	version := header[0]
	if version != _SOCKS_VERSION {
		conn.Write([]byte{_SOCKS_VERSION, _GENERAL_SOCKS_FAILURE})
		return nil, fmt.Errorf("unsupported SOCKS version: %d", version)
	}

	reply := header[1]
	switch reply {
	case _STATUS_OK:
	case _GENERAL_SOCKS_FAILURE:
		conn.Write([]byte{_SOCKS_VERSION, header[1]})
		return nil, fmt.Errorf("general SOCKS server failure")
	case _CONN_NOT_ALLOWED_BY_RULESET:
		conn.Write([]byte{_SOCKS_VERSION, header[1]})
		return nil, fmt.Errorf("connection not allowed by ruleset")
	case _NETWORK_UNREACHABLE:
		conn.Write([]byte{_SOCKS_VERSION, header[1]})
		return nil, fmt.Errorf("network unreachable")
	case _HOST_UNREACHABLE:
		conn.Write([]byte{_SOCKS_VERSION, header[1]})
		return nil, fmt.Errorf("host unreachable")
	case _CONN_REFUSED:
		conn.Write([]byte{_SOCKS_VERSION, header[1]})
		return nil, fmt.Errorf("connection refused")
	case _TTL_EXPIRED:
		conn.Write([]byte{_SOCKS_VERSION, header[1]})
		return nil, fmt.Errorf("TTL expired")
	case _COMMAND_NOT_SUPPORTED:
		conn.Write([]byte{_SOCKS_VERSION, header[1]})
		return nil, fmt.Errorf("command not supported")
	case _ADDRESS_TYPE_NOT_SUPPORTED:
		conn.Write([]byte{_SOCKS_VERSION, header[1]})
		return nil, fmt.Errorf("address type not supported")
	default:
		conn.Write([]byte{_SOCKS_VERSION, _GENERAL_SOCKS_FAILURE})
		return nil, fmt.Errorf("unknown reply: %d", reply)
	}

	addrLen := 0
	switch header[3] { // Address type
	case _IP_V4:
		addrLen = net.IPv4len
	case _DOMAIN_NAME:
		lengthByte := make([]byte, 1)
		if _, err := io.ReadFull(conn, lengthByte); err != nil {
			return nil, fmt.Errorf("error reading domain name length: %w", err)
		}
		addrLen = int(lengthByte[0])
	case _IP_V6:
		addrLen = net.IPv6len
	default:
		conn.Write([]byte{_SOCKS_VERSION, _ADDRESS_TYPE_NOT_SUPPORTED})
		return nil, fmt.Errorf("unknown address type: %d", header[3])
	}

	requestRest := make([]byte, addrLen+2) // +2 for port
	if _, err := io.ReadFull(conn, requestRest); err != nil {
		conn.Write([]byte{_SOCKS_VERSION, _GENERAL_SOCKS_FAILURE})
		return nil, fmt.Errorf("error reading the rest of the response: %w", err)
	}

	fullResponse := append(header, requestRest...)
	return fullResponse, nil
}

func contains[T comparable](slice []T, item T) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
