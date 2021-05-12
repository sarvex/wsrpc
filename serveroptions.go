package wsrpc

import (
	"crypto/ed25519"
)

// A ServerOption sets options such as credentials, codec and keepalive parameters, etc.
type ServerOption interface {
	apply(*serverOptions)
}

type serverOptions struct {
	// Buffer
	writeBufferSize int
	readBufferSize  int

	// Transport Credentials
	creds            TransportCredentials
	privKey          ed25519.PrivateKey
	clientIdentities map[[ed25519.PublicKeySize]byte]string
}

// funcServerOption wraps a function that modifies serverOptions into an
// implementation of the ServerOption interface.
type funcServerOption struct {
	f func(*serverOptions)
}

func newFuncServerOption(f func(*serverOptions)) *funcServerOption {
	return &funcServerOption{
		f: f,
	}
}

func (fdo *funcServerOption) apply(do *serverOptions) {
	fdo.f(do)
}

// Creds returns a ServerOption that sets credentials for server connections.
func Creds(privKey ed25519.PrivateKey, clientIdentities map[StaticSizePubKey]string) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		// Generate the TLS config for the client
		config := newServerTLSConfig(privKey, clientIdentities)

		o.creds = NewTransportCredentials(&config)
	})
}

// WriteBufferSize specifies the I/O write buffer size in bytes. If a buffer
// size is zero, then a useful default size is used.
func WriteBufferSize(s int) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.writeBufferSize = s
	})
}

// WriteBufferSize specifies the I/O read buffer size in bytes. If a buffer
// size is zero, then a useful default size is used.
func ReadBufferSize(s int) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.readBufferSize = s
	})
}

var defaultServerOptions = serverOptions{
	writeBufferSize: defaultWriteBufSize,
	readBufferSize:  defaultReadBufSize,
}