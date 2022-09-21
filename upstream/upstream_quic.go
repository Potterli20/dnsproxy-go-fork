package upstream

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/AdguardTeam/golibs/errors"
	"github.com/AdguardTeam/golibs/log"
	"github.com/Potterli20/dnsproxy-go-fork/proxyutil"
	"github.com/lucas-clemente/quic-go"
	"github.com/miekg/dns"
)

const (
	// QUICCodeNoError is used when the connection or stream needs to be closed,
	// but there is no error to signal.
	QUICCodeNoError = quic.ApplicationErrorCode(0)
	// QUICCodeInternalError signals that the DoQ implementation encountered
	// an internal error and is incapable of pursuing the transaction or the
	// connection.
	QUICCodeInternalError = quic.ApplicationErrorCode(1)
	// QUICKeepAlivePeriod is the value that we pass to *quic.Config and that
	// controls the period with with keep-alive frames are being sent to the
	// connection. We set it to 20s as it would be in the quic-go@v0.27.1 with
	// KeepAlive field set to true This value is specified in
	// https://pkg.go.dev/github.com/lucas-clemente/quic-go/internal/protocol#MaxKeepAliveInterval.
	//
	// TODO(ameshkov):  Consider making it configurable.
	QUICKeepAlivePeriod = time.Second * 20
)

// dnsOverQUIC is a struct that implements the Upstream interface for the
// DNS-over-QUIC protocol (spec: https://www.rfc-editor.org/rfc/rfc9250.html).
type dnsOverQUIC struct {
	// boot is a bootstrap DNS abstraction that is used to resolve the upstream
	// server's address and open a network connection to it.
	boot *bootstrapper
	// quicConfig is the QUIC configuration that is used for establishing
	// connections to the upstream.  This configuration includes the TokenStore
	// that needs to be stored for the lifetime of dnsOverQUIC since we can
	// re-create the connection.
	quicConfig *quic.Config
	// conn is the current active QUIC connection.  It can be closed and
	// re-opened when needed.
	conn quic.Connection
	// connGuard protects conn and quicConfig.
	connGuard sync.RWMutex
	// bytesPool is a *sync.Pool we use to store byte buffers in.  These byte
	// buffers are used to read responses from the upstream.
	bytesPool      *sync.Pool
	bytesPoolGuard sync.Mutex
}

// type check
var _ Upstream = &dnsOverQUIC{}

// newDoQ returns the DNS-over-QUIC Upstream.
func newDoQ(uu *url.URL, opts *Options) (u Upstream, err error) {
	addPort(uu, defaultPortDoQ)

	var b *bootstrapper
	b, err = urlToBoot(uu, opts)
	if err != nil {
		return nil, fmt.Errorf("creating quic bootstrapper: %w", err)
	}

	return &dnsOverQUIC{
		boot: b,
		quicConfig: &quic.Config{
			KeepAlivePeriod: QUICKeepAlivePeriod,
			TokenStore:      newQUICTokenStore(),
		},
	}, nil
}

// Address implements the Upstream interface for *dnsOverQUIC.
func (p *dnsOverQUIC) Address() string { return p.boot.URL.String() }

// Exchange implements the Upstream interface for *dnsOverQUIC.
func (p *dnsOverQUIC) Exchange(m *dns.Msg) (resp *dns.Msg, err error) {
	// When sending queries over a QUIC connection, the DNS Message ID MUST be
	// set to zero.
	id := m.Id
	m.Id = 0
	defer func() {
		// Restore the original ID to not break compatibility with proxies.
		m.Id = id
		if resp != nil {
			resp.Id = id
		}
	}()

	// Check if there was already an active conn before sending the request.
	// We'll only attempt to re-connect if there was one.
	hasConnection := p.hasConnection()

	// Make the first attempt to send the DNS query.
	resp, err = p.exchangeQUIC(m)

	// Make up to 2 attempts to re-open the QUIC connection and send the request
	// again.  There are several cases where this workaround is necessary to
	// make DoQ usable.  We need to make 2 attempts in the case when the
	// connection was closed (due to inactivity for example) AND the server
	// refuses to open a 0-RTT connection.
	for i := 0; hasConnection && p.shouldRetry(err) && i < 2; i++ {
		log.Debug("re-creating the QUIC connection and retrying due to %v", err)

		// Close the active connection to make sure we'll try to re-connect.
		p.closeConnWithError(QUICCodeNoError)

		// Retry sending the request.
		resp, err = p.exchangeQUIC(m)
	}

	if err != nil {
		// If we're unable to exchange messages, make sure the connection is
		// closed and signal about an internal error.
		p.closeConnWithError(QUICCodeInternalError)
	}

	return resp, err
}

// exchangeQUIC attempts to open a QUIC connection, send the DNS message
// through it and return the response it got from the server.
func (p *dnsOverQUIC) exchangeQUIC(m *dns.Msg) (resp *dns.Msg, err error) {
	var conn quic.Connection
	conn, err = p.getConnection(true)
	if err != nil {
		return nil, err
	}

	var buf []byte
	buf, err = m.Pack()
	if err != nil {
		return nil, fmt.Errorf("failed to pack DNS message for DoQ: %w", err)
	}

	var stream quic.Stream
	stream, err = p.openStream(conn)
	if err != nil {
		return nil, err
	}

	_, err = stream.Write(proxyutil.AddPrefix(buf))
	if err != nil {
		return nil, fmt.Errorf("failed to write to a QUIC stream: %w", err)
	}

	// The client MUST send the DNS query over the selected stream, and MUST
	// indicate through the STREAM FIN mechanism that no further data will
	// be sent on that stream. Note, that stream.Close() closes the
	// write-direction of the stream, but does not prevent reading from it.
	_ = stream.Close()

	return p.readMsg(stream)
}

// shouldRetry checks what error we received and decides whether it is required
// to re-open the connection and retry sending the request.
func (p *dnsOverQUIC) shouldRetry(err error) (ok bool) {
	return isQUICRetryError(err)
}

// getBytesPool returns (creates if needed) a pool we store byte buffers in.
func (p *dnsOverQUIC) getBytesPool() (pool *sync.Pool) {
	p.bytesPoolGuard.Lock()
	defer p.bytesPoolGuard.Unlock()

	if p.bytesPool == nil {
		p.bytesPool = &sync.Pool{
			New: func() any {
				b := make([]byte, dns.MaxMsgSize)

				return &b
			},
		}
	}

	return p.bytesPool
}

// getConnection opens or returns an existing quic.Connection. useCached
// argument controls whether we should try to use the existing cached
// connection.  If it is false, we will forcibly create a new connection and
// close the existing one if needed.
func (p *dnsOverQUIC) getConnection(useCached bool) (quic.Connection, error) {
	var conn quic.Connection
	p.connGuard.RLock()
	conn = p.conn
	if conn != nil && useCached {
		p.connGuard.RUnlock()

		return conn, nil
	}
	if conn != nil {
		// we're recreating the connection, let's create a new one.
		_ = conn.CloseWithError(QUICCodeNoError, "")
	}
	p.connGuard.RUnlock()

	p.connGuard.Lock()
	defer p.connGuard.Unlock()

	var err error
	conn, err = p.openConnection()
	if err != nil {
		return nil, err
	}
	p.conn = conn

	return conn, nil
}

// hasConnection returns true if there's an active QUIC connection.
func (p *dnsOverQUIC) hasConnection() (ok bool) {
	p.connGuard.Lock()
	defer p.connGuard.Unlock()

	return p.conn != nil
}

// openStream opens a new QUIC stream for the specified connection.
func (p *dnsOverQUIC) openStream(conn quic.Connection) (quic.Stream, error) {
	ctx, cancel := p.boot.newContext()
	defer cancel()

	stream, err := conn.OpenStreamSync(ctx)
	if err == nil {
		return stream, nil
	}

	// We can get here if the old QUIC connection is not valid anymore.  We
	// should try to re-create the connection again in this case.
	newConn, err := p.getConnection(false)
	if err != nil {
		return nil, err
	}
	// Open a new stream.
	return newConn.OpenStreamSync(ctx)
}

// openConnection opens a new QUIC connection.
func (p *dnsOverQUIC) openConnection() (conn quic.Connection, err error) {
	tlsConfig, dialContext, err := p.boot.get()
	if err != nil {
		return nil, fmt.Errorf("failed to bootstrap QUIC connection: %w", err)
	}

	// we're using bootstrapped address instead of what's passed to the function
	// it does not create an actual connection, but it helps us determine
	// what IP is actually reachable (when there're v4/v6 addresses).
	rawConn, err := dialContext(context.Background(), "udp", "")
	if err != nil {
		return nil, fmt.Errorf("failed to open a QUIC connection: %w", err)
	}
	// It's never actually used
	_ = rawConn.Close()

	udpConn, ok := rawConn.(*net.UDPConn)
	if !ok {
		return nil, fmt.Errorf("failed to open connection to %s", p.Address())
	}

	addr := udpConn.RemoteAddr().String()

	ctx, cancel := p.boot.newContext()
	defer cancel()

	conn, err = quic.DialAddrEarlyContext(ctx, addr, tlsConfig, p.quicConfig)
	if err != nil {
		return nil, fmt.Errorf("opening quic connection to %s: %w", p.Address(), err)
	}

	return conn, nil
}

// closeConnWithError closes the active connection with error to make sure that
// new queries were processed in another connection. We can do that in the case
// of a fatal error.
func (p *dnsOverQUIC) closeConnWithError(code quic.ApplicationErrorCode) {
	p.connGuard.Lock()
	defer p.connGuard.Unlock()

	if p.conn == nil {
		// Do nothing, there's no active conn anyways.
		return
	}

	err := p.conn.CloseWithError(code, "")
	if err != nil {
		log.Error("failed to close the conn: %v", err)
	}
	p.conn = nil

	// Re-create the token store to make sure we're not trying to use invalid
	// tokens for 0-RTT.
	p.quicConfig.TokenStore = newQUICTokenStore()
}

// readMsg reads the incoming DNS message from the QUIC stream.
func (p *dnsOverQUIC) readMsg(stream quic.Stream) (m *dns.Msg, err error) {
	pool := p.getBytesPool()
	bufPtr := pool.Get().(*[]byte)

	defer pool.Put(bufPtr)

	respBuf := *bufPtr
	n, err := stream.Read(respBuf)
	if err != nil && n == 0 {
		return nil, fmt.Errorf("reading response from %s: %w", p.Address(), err)
	}

	// All DNS messages (queries and responses) sent over DoQ connections MUST
	// be encoded as a 2-octet length field followed by the message content as
	// specified in [RFC1035].
	// IMPORTANT: Note, that we ignore this prefix here as this implementation
	// does not support receiving multiple messages over a single connection.
	m = new(dns.Msg)
	err = m.Unpack(respBuf[2:])
	if err != nil {
		return nil, fmt.Errorf("unpacking response from %s: %w", p.Address(), err)
	}

	return m, nil
}

// newQUICTokenStore creates a new quic.TokenStore that is necessary to have
// in order to benefit from 0-RTT.
func newQUICTokenStore() (s quic.TokenStore) {
	// You can read more on address validation here:
	// https://datatracker.ietf.org/doc/html/rfc9000#section-8.1
	// Setting maxOrigins to 1 and tokensPerOrigin to 10 assuming that this is
	// more than enough for the way we use it (one connection per upstream).
	return quic.NewLRUTokenStore(1, 10)
}

// isQUICRetryError checks the error and determines whether it may signal that
// we should re-create the QUIC connection.  This requirement is caused by
// quic-go issues, see the comments inside this function.
// TODO(ameshkov): re-test when updating quic-go.
func isQUICRetryError(err error) (ok bool) {
	var qAppErr *quic.ApplicationError
	if errors.As(err, &qAppErr) && qAppErr.ErrorCode == 0 {
		// This error is often returned when the server has been restarted,
		// and we try to use the same connection on the client-side. It seems,
		// that the old connections aren't closed immediately on the server-side
		// and that's why one can run into this.
		// In addition to that, quic-go HTTP3 client implementation does not
		// clean up dead connections (this one is specific to DoH3 upstream):
		// https://github.com/lucas-clemente/quic-go/issues/765
		return true
	}

	var qIdleErr *quic.IdleTimeoutError
	if errors.As(err, &qIdleErr) {
		// This error means that the connection was closed due to being idle.
		// In this case we should forcibly re-create the QUIC connection.
		// Reproducing is rather simple, stop the server and wait for 30 seconds
		// then try to send another request via the same upstream.
		return true
	}

	if errors.Is(err, quic.Err0RTTRejected) {
		// This error happens when we try to establish a 0-RTT connection with
		// a token the server is no more aware of.  This can be reproduced by
		// restarting the QUIC server (it will clear its tokens cache).  The
		// next connection attempt will return this error until the client's
		// tokens cache is purged.
		return true
	}

	return false
}
