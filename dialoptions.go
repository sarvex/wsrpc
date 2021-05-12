package wsrpc

import (
	"crypto/ed25519"

	"github.com/smartcontractkit/wsrpc/internal/backoff"
)

// dialOptions configure a Dial call. dialOptions are set by the DialOption
// values passed to Dial.
type dialOptions struct {
	copts ConnectOptions
	bs    backoff.Strategy
}

// DialOption configures how we set up the connection.
type DialOption interface {
	apply(*dialOptions)
}

// funcDialOption wraps a function that modifies dialOptions into an
// implementation of the DialOption interface.
type funcDialOption struct {
	f func(*dialOptions)
}

func (fdo *funcDialOption) apply(do *dialOptions) {
	fdo.f(do)
}

func newFuncDialOption(f func(*dialOptions)) *funcDialOption {
	return &funcDialOption{
		f: f,
	}
}

// WithTransportCredentials returns a DialOption which configures a connection
// level security credentials (e.g., TLS/SSL).
func WithTransportCreds(privKey ed25519.PrivateKey, serverPubKey [ed25519.PublicKeySize]byte) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		// Generate the TLS config for the client
		config := newClientTLSConfig(privKey, map[StaticSizePubKey]string{
			serverPubKey: "server",
		})

		o.copts.TransportCredentials = NewTransportCredentials(&config)
	})
}

func defaultDialOptions() dialOptions {
	return dialOptions{
		copts: ConnectOptions{
			// 	WriteBufferSize: defaultWriteBufSize,
			// 	ReadBufferSize:  defaultReadBufSize,
		},
	}
}