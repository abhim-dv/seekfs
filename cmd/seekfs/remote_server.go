package main

// Phase 2 (Mode L): versioned length-prefixed JSON framing over a loopback TCP
// listener, connection-scoped request IDs, and the sanitized remote response
// projection.  See docs/NETWORK_CLIENT_SERVER_IMPLEMENTATION_SCOPE.md §7 and §8.
//
// The loopback listener is DISABLED by default and must be explicitly enabled
// (e.g. -remote-addr 127.0.0.1:port).  It is the development/proof-of-transport
// path only and is never a privilege boundary: an unauthenticated loopback
// listener is reachable by other local sessions.  Production LAN exposure
// (Mode N) ships in Phase 3 with the broker + auth.

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// remoteProtocolVersion is the wire protocol version for the Mode L transport.
const remoteProtocolVersion = 1

// remoteMaxFrameBytes bounds a single frame; larger frames close the
// connection.
const remoteMaxFrameBytes = 16 * 1024 * 1024

// remoteConnMaxInFlight bounds outstanding requests per remote connection.
const remoteConnMaxInFlight = 32

// remoteFrameType enumerates the frame kinds in the remote envelope.
type remoteFrameType string

const (
	remoteFrameHello    remoteFrameType = "hello"
	remoteFrameRequest  remoteFrameType = "request"
	remoteFrameResponse remoteFrameType = "response"
	remoteFrameCancel   remoteFrameType = "cancel"
	remoteFrameError    remoteFrameType = "error"
)

// remoteFrame is the length-prefixed JSON envelope.  payload carries the
// existing serviceRequest / serviceResponse JSON body verbatim.
type remoteFrame struct {
	Type    remoteFrameType `json:"type"`
	ID      int64           `json:"id,omitempty"`
	V       int             `json:"v"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// remoteHello carries server capability info returned to a client after a
// successful hello.  It is deliberately minimal: no volumes, roots, remaps, or
// ACL-mode details pre-authentication (Phase 3 adds the authenticated
// handshake).
type remoteHello struct {
	Version string   `json:"version"`
	Commit  string   `json:"commit"`
	Proto   int      `json:"proto"`
	Mode    string   `json:"mode"` // "loopback"
	Modes   []string `json:"modes"`
}

// remoteLoopbackServer serves the Mode L transport for one loopback address.
type remoteLoopbackServer struct {
	s     *goSearchService
	addr  string
	ln    net.Listener
	mu    sync.Mutex
	conns map[*remoteConn]struct{}
	stop  chan struct{}
	done  chan struct{}
}

// newRemoteLoopbackServer creates a Mode L server bound to addr (which must be a
// loopback address).  The caller owns starting it.
func newRemoteLoopbackServer(s *goSearchService, addr string) *remoteLoopbackServer {
	return &remoteLoopbackServer{
		s:     s,
		addr:  addr,
		conns: make(map[*remoteConn]struct{}),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

// start binds the loopback listener and begins accepting connections.  It
// returns an error if the address cannot be bound or is not loopback.
func (rs *remoteLoopbackServer) start() error {
	host, _, err := net.SplitHostPort(rs.addr)
	if err != nil {
		return fmt.Errorf("remote addr %q: %w", rs.addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("remote addr %q is not a loopback address; Mode L must bind 127.0.0.1/::1", rs.addr)
	}
	ln, err := net.Listen("tcp", rs.addr)
	if err != nil {
		return fmt.Errorf("remote loopback listen %s: %w", rs.addr, err)
	}
	rs.ln = ln
	go rs.acceptLoop()
	return nil
}

func (rs *remoteLoopbackServer) acceptLoop() {
	defer close(rs.done)
	for {
		conn, err := rs.ln.Accept()
		if err != nil {
			select {
			case <-rs.stop:
				return
			default:
				return
			}
		}
		rc := &remoteConn{rs: rs, conn: conn, inFlight: make(map[int64]*remoteRequestState)}
		rs.mu.Lock()
		rs.conns[rc] = struct{}{}
		rs.mu.Unlock()
		go rc.serve()
	}
}

// close shuts down the listener and all active connections.
func (rs *remoteLoopbackServer) close() {
	close(rs.stop)
	if rs.ln != nil {
		rs.ln.Close()
	}
	rs.mu.Lock()
	conns := make([]*remoteConn, 0, len(rs.conns))
	for rc := range rs.conns {
		conns = append(conns, rc)
	}
	rs.mu.Unlock()
	for _, rc := range conns {
		rc.close()
	}
	<-rs.done
}

// remoteRequestState tracks one in-flight remote request for connection-scoped
// cancellation.
type remoteRequestState struct {
	cancel func()
}

// remoteConn is a single client connection on the Mode L transport.
type remoteConn struct {
	rs        *remoteLoopbackServer
	conn      net.Conn
	mu        sync.Mutex      // serializes writes
	inFlight  map[int64]*remoteRequestState
	closeOnce sync.Once
}

// close tears down the connection.
func (rc *remoteConn) close() {
	rc.closeOnce.Do(func() {
		rc.conn.Close()
		rc.rs.mu.Lock()
		delete(rc.rs.conns, rc)
		rc.rs.mu.Unlock()
	})
}

// writeFrame serializes a frame to the connection under the write lock.
func (rc *remoteConn) writeFrame(f remoteFrame) error {
	payload, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(payload) > remoteMaxFrameBytes {
		return errors.New("response frame exceeds max frame size")
	}
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(len(payload)))
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if _, err := rc.conn.Write(buf[:]); err != nil {
		return err
	}
	_, err = rc.conn.Write(payload)
	return err
}

// serve reads frames until the connection closes.  Requests are dispatched with
// a remote principal (read-only, non-mutating) and the sanitized projection.
func (rc *remoteConn) serve() {
	defer rc.close()
	r := bufio.NewReader(rc.conn)
	for {
		select {
		case <-rc.rs.stop:
			return
		default:
		}
		payload, err := readRemoteFramePayload(r)
		if err != nil {
			if err != io.EOF {
				_ = rc.writeFrame(remoteFrame{Type: remoteFrameError, V: remoteProtocolVersion, Payload: mustJSON(serviceResponse{OK: false, Message: err.Error()})})
			}
			return
		}
		var f remoteFrame
		if err := json.Unmarshal(payload, &f); err != nil {
			_ = rc.writeFrame(remoteFrame{Type: remoteFrameError, V: remoteProtocolVersion, Payload: mustJSON(serviceResponse{OK: false, Message: "malformed frame: " + err.Error()})})
			return
		}
		if f.V != remoteProtocolVersion {
			_ = rc.writeFrame(remoteFrame{Type: remoteFrameError, V: remoteProtocolVersion, Payload: mustJSON(serviceResponse{OK: false, Message: fmt.Sprintf("unsupported protocol version %d (server v%d)", f.V, remoteProtocolVersion)})})
			return
		}
		switch f.Type {
		case remoteFrameHello:
			rc.handleHello(f.ID)
		case remoteFrameRequest:
			rc.handleRequest(f.ID, f.Payload)
		case remoteFrameCancel:
			rc.handleCancel(f.ID)
		default:
			_ = rc.writeFrame(remoteFrame{Type: remoteFrameError, V: remoteProtocolVersion, ID: f.ID, Payload: mustJSON(serviceResponse{OK: false, Message: "unexpected frame type " + string(f.Type)})})
		}
	}
}

func (rc *remoteConn) handleHello(id int64) {
	hello := remoteHello{
		Version: version,
		Commit:  commit,
		Proto:   remoteProtocolVersion,
		Mode:    "loopback",
		Modes:   []string{"loopback"},
	}
	_ = rc.writeFrame(remoteFrame{Type: remoteFrameResponse, V: remoteProtocolVersion, ID: id, Payload: mustJSON(hello)})
}

// handleRequest dispatches a remote request with read-only remote capabilities
// and applies the sanitized remote projection to the response.  The request id
// is connection-scoped for cancellation.
func (rc *remoteConn) handleRequest(id int64, payload json.RawMessage) {
	var req serviceRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		_ = rc.writeFrame(remoteFrame{Type: remoteFrameResponse, V: remoteProtocolVersion, ID: id, Payload: mustJSON(sanitizeRemoteResponse(serviceResponse{OK: false, Message: "bad request: " + err.Error()}))})
		return
	}

	// Bound in-flight work per connection.
	rc.mu.Lock()
	if len(rc.inFlight) >= remoteConnMaxInFlight {
		rc.mu.Unlock()
		_ = rc.writeFrame(remoteFrame{Type: remoteFrameResponse, V: remoteProtocolVersion, ID: id, Payload: mustJSON(sanitizeRemoteResponse(serviceResponse{OK: false, Message: "busy: too many in-flight requests"}))})
		return
	}
	state := &remoteRequestState{}
	rc.inFlight[id] = state
	rc.mu.Unlock()

	// Remote callers are read-only.  watch-delta is deferred remotely until
	// Phase 7; the capability gate rejects it here.
	caps := serviceCapabilities{ReadOnly: true, Remote: true}
	principal := servicePrincipal{}

	// Server-owned deadline clamp: a remote client cannot drive an unbounded
	// query by supplying a large deadline_unix.
	req.DeadlineUnix = clampRemoteDeadline(req.DeadlineUnix)

	// The request_seq field drives the service-global cancellation counter and
	// must not be trusted from a remote caller (it could cancel another
	// client's or the local GUI's in-flight query).  Remote cancellation is
	// connection-scoped via the frame id, so clear it here.
	req.RequestSeq = 0

	// Dispatch to the shared engine handler, capturing the response.  The
	// handler writes a single serviceResponse to the provided writer.
	resp := rc.dispatch(caps, principal, &req)

	rc.mu.Lock()
	delete(rc.inFlight, id)
	rc.mu.Unlock()

	_ = rc.writeFrame(remoteFrame{Type: remoteFrameResponse, V: remoteProtocolVersion, ID: id, Payload: mustJSON(sanitizeRemoteResponse(resp))})
}

// dispatch runs handleServiceCommand and returns the encoded response by
// capturing the handler's output.
func (rc *remoteConn) dispatch(caps serviceCapabilities, principal servicePrincipal, req *serviceRequest) serviceResponse {
	var buf bytesBuffer
	rc.rs.s.handleServiceCommand(&buf, principal, caps, req)
	var resp serviceResponse
	if err := json.Unmarshal(buf.b, &resp); err != nil {
		return serviceResponse{OK: false, Message: "internal dispatch error: " + err.Error()}
	}
	return resp
}

// handleCancel cancels the in-flight request with the matching connection-scoped
// id (if any).
func (rc *remoteConn) handleCancel(id int64) {
	rc.mu.Lock()
	state, ok := rc.inFlight[id]
	rc.mu.Unlock()
	if ok && state != nil && state.cancel != nil {
		state.cancel()
	}
}

// bytesBuffer is a minimal writer used to capture a single handler response.
type bytesBuffer struct{ b []byte }

func (b *bytesBuffer) Write(p []byte) (int, error) {
	b.b = append(b.b, p...)
	return len(p), nil
}

// readRemoteFramePayload reads one 4-byte big-endian length-prefixed JSON frame.
func readRemoteFramePayload(r *bufio.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > remoteMaxFrameBytes {
		return nil, fmt.Errorf("frame length %d out of range", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// sanitizeRemoteResponse applies the single remote-safe response projection to
// every operation: it strips physical server internals and detailed trace data,
// keeping only public results and coarse health.
func sanitizeRemoteResponse(resp serviceResponse) serviceResponse {
	resp.PID = 0
	resp.Executable = ""
	resp.ExecutableHash = ""
	resp.PipeName = ""
	resp.Runtime = nil
	resp.ProcessMode = ""
	// Coarse diagnostics are fine; detailed planner trace internals are not.
	resp.BlocksDecoded = 0
	resp.BlocksSkipped = 0
	resp.ScalarDriver = ""
	resp.ScalarInterval = 0
	resp.RecordsVerified = 0
	resp.ComponentDriver = ""
	resp.ComponentRoots = 0
	resp.ComponentIntervals = 0
	resp.ComponentCardinality = 0
	resp.ComponentSelfHits = 0
	resp.ComponentBounds = ""
	resp.ComponentRecordsVerified = 0
	resp.FilenameDriver = ""
	resp.FilenameRequiredGrams = 0
	resp.FilenamePostingHint = 0
	resp.FilenameRecordsVerified = 0
	resp.OverlayBaseWindow = 0
	resp.PostingPrefetchBytes = 0
	resp.PostingPrefetchRanges = 0
	resp.PostingPrefetchPages = 0
	resp.Terms = nil
	resp.Declines = nil
	// Sanitize the dbInfo list: keep volume/state/health, drop physical paths,
	// journal ids, memory, and error detail that leaks server internals.
	if resp.DBs != nil {
		dbs := make([]dbInfo, 0, len(resp.DBs))
		for _, info := range resp.DBs {
			info.Path = ""
			info.JournalID = 0
			info.Checkpoint = 0
			info.StaleReason = ""
			info.PersistFailures = 0
			info.LastPersistError = ""
			info.LastReplayError = ""
			info.LastReplayNext = 0
			info.QueryExtKeys = 0
			info.QueryDirs = 0
			info.DerivedSections = nil
			info.DerivedBytes = 0
			info.Memory = nil
			dbs = append(dbs, info)
		}
		resp.DBs = dbs
	}
	return resp
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"ok":false,"message":"internal marshal error"}`)
	}
	return b
}

// clampRemoteDeadline caps a client-supplied deadline at the server max so a
// remote caller cannot drive an unbounded query.
func clampRemoteDeadline(deadlineUnix int64) int64 {
	if deadlineUnix <= 0 {
		return time.Now().Add(serviceQueryTimeout - 250*time.Millisecond).UnixNano()
	}
	maxDeadline := time.Now().Add(serviceQueryTimeout).UnixNano()
	if deadlineUnix > maxDeadline {
		return maxDeadline
	}
	return deadlineUnix
}
