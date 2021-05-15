package wsrpc

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/smartcontractkit/wsrpc/connectivity"
	"github.com/smartcontractkit/wsrpc/internal/backoff"
	"github.com/smartcontractkit/wsrpc/internal/message"
	"github.com/smartcontractkit/wsrpc/internal/wsrpcsync"
	"google.golang.org/protobuf/proto"
)

var (
	// errConnClosing indicates that the connection is closing.
	errConnClosing = errors.New("grpc: the connection is closing")
)

type ClientConnInterface interface {
	Invoke(method string, args interface{}, reply interface{}) error
}

// ClientConn represents a virtual connection to a websocket endpoint, to
// perform RPCs.
type ClientConn struct {
	ctx context.Context
	mu  sync.RWMutex

	target string
	csCh   <-chan connectivity.State

	dopts dialOptions
	conn  *addrConn

	// readFn contains the registered handler for reading messages
	readFn func(message []byte)

	// Contains all pending method call ids and the channel to respond to when
	// a result is received
	methodCalls map[string]chan<- []byte
}

// Dial creates a client connection to the given target.
func Dial(target string, opts ...DialOption) (*ClientConn, error) {
	cc := &ClientConn{
		ctx:         context.Background(),
		target:      target,
		dopts:       defaultDialOptions(),
		methodCalls: map[string]chan<- []byte{},
	}

	for _, opt := range opts {
		opt.apply(&cc.dopts)
	}

	// Set the backoff strategy. We may need to consider making this
	// customizable in the dial options.
	cc.dopts.bs = backoff.DefaultExponential

	addrConn, err := cc.newAddrConn(target)
	if err != nil {
		return nil, errors.New("Could not establish a connection")
	}

	addrConn.connect()
	cc.conn = addrConn

	return cc, nil
}

// newAddrConn creates an addrConn for the addr and sets it to cc.conn.
func (cc *ClientConn) newAddrConn(addr string) (*addrConn, error) {
	csCh := make(chan connectivity.State)
	ac := &addrConn{
		state:   connectivity.Idle,
		stateCh: csCh,
		cc:      cc,
		addr:    addr,
		dopts:   cc.dopts,
	}
	ac.ctx, ac.cancel = context.WithCancel(cc.ctx)
	cc.mu.Lock()

	cc.conn = ac
	cc.csCh = csCh
	cc.mu.Unlock()

	go cc.listenForRead()

	return ac, nil
}

// listenForRead listens for the connectivty state to be ready and enables the
// read handler
func (cc *ClientConn) listenForRead() {
	for {
		s := <-cc.csCh

		var done chan struct{}

		if s == connectivity.Ready {
			done := make(chan struct{})
			go cc.handleRead(done)
		} else {
			if done != nil {
				close(done)
			}
		}
	}
}

// handleRead listens to the transport read channel and passes the message to the
// readFn handler.
func (cc *ClientConn) handleRead(done <-chan struct{}) {
	for {
		select {
		case in := <-cc.conn.transport.Read():
			// Unmarshal the message
			msg := &message.Message{}
			if err := UnmarshalProtoMessage(in, msg); err != nil {
				log.Fatalln("Failed to parse message:", err)
				continue
			}

			switch ex := msg.Exchange.(type) {
			case *message.Message_Request:
				fmt.Println("Request:", msg)
			case *message.Message_Response:
				cc.handleMessageResponse(ex.Response)
			default:
				log.Println("Invalid message type")
			}
		case <-done:
			return
		}
	}
}

// handleMessageResponse finds the call which matches the method call id of the
// response and sends the payload to the call channel.
func (cc *ClientConn) handleMessageResponse(r *message.Response) {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	callID := r.GetCallId()
	if call, ok := cc.methodCalls[callID]; ok {
		call <- r.GetPayload()

		cc.removeMethodCall(callID) // Delete the call now that we have completed the request/response cycle
	}
}

// Close tears down the ClientConn and all underlying connections.
func (cc *ClientConn) Close() {
	conn := cc.conn

	cc.mu.Lock()
	cc.conn = nil
	cc.mu.Unlock()

	conn.teardown()
}

func (cc *ClientConn) Invoke(method string, args interface{}, reply interface{}) error {
	// Ensure the connection state is ready
	if cc.conn.state != connectivity.Ready {
		return errors.New("connection is not ready")
	}

	// Convert the args proto into bytes to insert in the message
	payload, err := MarshalProtoMessage(args)
	if err != nil {
		return err
	}

	// Build the message
	callID := uuid.NewString()
	msg := &message.Message{
		Exchange: &message.Message_Request{
			Request: &message.Request{
				CallId:  callID,
				Method:  method,
				Payload: payload,
			},
		},
	}

	msgB, err := proto.Marshal(msg)
	if err != nil {
		return err
	}

	cc.mu.Lock()
	wait := cc.registerMethodCall(callID)
	cc.mu.Unlock()

	cc.conn.transport.Write(msgB)

	// Wait for the response
	select {
	case b := <-wait:
		// Unmarshal the payload into the reply
		err := UnmarshalProtoMessage(b, reply)
		if err != nil {
			return err
		}
	case <-time.After(2 * time.Second): // TODO - Make this configurable
		// Remove the call since we have timeout
		cc.mu.Lock()
		cc.removeMethodCall(callID)
		cc.mu.Unlock()
		return errors.New("call timeout")
	}

	return nil
}

// registerMethodCall registers a method call to the method call map.
//
// This requires a lock on cc.mu.
func (cc *ClientConn) registerMethodCall(id string) <-chan []byte {
	wait := make(chan []byte)
	cc.methodCalls[id] = wait

	return wait
}

// removeMethodCall deregisters a method call to the method call map.
//
// This requires a lock on cc.mu.
func (cc *ClientConn) removeMethodCall(id string) {
	delete(cc.methodCalls, id)
}

// addrConn is a network connection to a given address.
type addrConn struct {
	ctx    context.Context
	cancel context.CancelFunc

	cc *ClientConn

	addr  string
	dopts dialOptions

	// transport is set when there's a viable transport, and is reset
	// to nil when the current transport should no longer be used (e.g.
	// after transport is closed, ac has been torn down).
	transport ClientTransport // The current transport.

	mu sync.Mutex

	// Use updateConnectivityState for updating addrConn's connectivity state.
	state connectivity.State
	// Notifies this channel when the ConnectivityState changes
	stateCh chan connectivity.State
}

// connect starts creating a transport.
// It does nothing if the ac is not IDLE.
func (ac *addrConn) connect() error {
	ac.mu.Lock()
	if ac.state == connectivity.Shutdown {
		ac.mu.Unlock()
		return errConnClosing
	}

	if ac.state != connectivity.Idle {
		ac.mu.Unlock()
		return nil
	}

	// Update connectivity state within the lock to prevent subsequent or
	// concurrent calls from resetting the transport more than once.
	ac.updateConnectivityState(connectivity.Connecting)
	ac.mu.Unlock()

	// Start a goroutine connecting to the server asynchronously.
	go ac.resetTransport()

	return nil
}

// Note: this requires a lock on ac.mu.
func (ac *addrConn) updateConnectivityState(s connectivity.State) {
	if ac.state == s {
		return
	}
	ac.state = s
	ac.stateCh <- s
	log.Printf("[AddrConn] Connectivity State: %s", s)
}

// resetTransport attempts to connect to the server. If the connection fails,
// it will continously attempt reconnection with an exponential backoff.
func (ac *addrConn) resetTransport() {
	for i := 0; ; i++ {
		ac.mu.Lock()
		if ac.state == connectivity.Shutdown {
			ac.mu.Unlock()
			return
		}

		backoffFor := ac.dopts.bs.NextBackOff()
		addr := ac.addr
		copts := ac.dopts.copts

		ac.transport = nil

		ac.updateConnectivityState(connectivity.Connecting)
		ac.mu.Unlock()

		newTr, reconnect, err := ac.createTransport(addr, copts)
		if err != nil {
			log.Println(err)

			// After connection failure, the addrConn enters TRANSIENT_FAILURE.
			ac.mu.Lock()
			if ac.state == connectivity.Shutdown {
				ac.mu.Unlock()
				return
			}
			ac.updateConnectivityState(connectivity.TransientFailure)
			ac.mu.Unlock()

			// Backoff.
			timer := time.NewTimer(backoffFor)
			log.Printf("[AddrConn] Waiting %s to reconnect", backoffFor)
			select {
			case <-timer.C:
				// NOOP - This falls through to continue to retry connecting
			case <-ac.ctx.Done():
				fmt.Println("Context Cancelled")
				timer.Stop()
				return
			}
			continue
		}

		// Close the transport early if in a SHUTDOWN state
		ac.mu.Lock()
		if ac.state == connectivity.Shutdown {
			ac.mu.Unlock()
			newTr.Close()
			return
		}
		ac.transport = newTr
		ac.dopts.bs.Reset()

		ac.updateConnectivityState(connectivity.Ready)

		ac.mu.Unlock()

		// Block until the created transport is down. When this happens, we
		// attempt to reconnect by starting again from the top
		<-reconnect.Done()
	}
}

// createTransport creates a new transport. If it fails to connect to the server,
// it returns an error which used to detect whether a retry is necessary. This
// also returns a reconnect event which is fired when the transport closes due
// to issues with the underlying connection.
func (ac *addrConn) createTransport(addr string, copts ConnectOptions) (ClientTransport, *wsrpcsync.Event, error) {
	reconnect := wsrpcsync.NewEvent()
	once := sync.Once{}

	// Called when the transport closes
	onClose := func() {
		ac.mu.Lock()
		once.Do(func() {
			if ac.state == connectivity.Ready {
				ac.updateConnectivityState(connectivity.Idle)
			}
		})
		ac.mu.Unlock()
		reconnect.Fire()
	}

	tr, err := NewWebsocketClient(ac.cc.ctx, addr, copts, onClose)

	return tr, reconnect, err
}

// tearDown starts to tear down the addrConn.
func (ac *addrConn) teardown() {
	ac.mu.Lock()

	if ac.state == connectivity.Shutdown {
		ac.mu.Unlock()
		return
	}

	ac.updateConnectivityState(connectivity.Shutdown)

	curTr := ac.transport
	ac.transport = nil

	ac.cancel()
	if curTr != nil {
		curTr.Close()
	}

	ac.mu.Unlock()
}
