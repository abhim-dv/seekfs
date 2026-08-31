package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSanitizeRemoteResponse(t *testing.T) {
	resp := serviceResponse{
		OK:             true,
		Count:          3,
		SearchMS:       12.5,
		Source:         "global-name",
		PlannerMode:    "global-name",
		PID:            1234,
		Executable:     "C:\\ProgramData\\seekfs\\seekfs.exe",
		ExecutableHash: "abc123",
		PipeName:       `\\.\pipe\seekfs-service`,
		ProcessMode:    "windows-service",
		Version:        "1.6.0",
		Commit:         "abc",
		Date:           "today",
		BuildFlavor:    "release",
		BlocksDecoded:  99,
		ScalarDriver:   "x",
		ComponentBounds: "secret-bounds",
		Terms:          []traceTerm{{Term: "x", Kind: "y"}},
		Declines:       []traceDecline{{Source: "s", Reason: "r"}},
		Runtime:        &runtimeMemoryInfo{HeapAllocBytes: 1},
		Results:        []string{"C:\\foo\\bar.txt"},
		Rows:           []jsonResult{{Path: "C:\\foo\\bar.txt"}},
		DBs: []dbInfo{
			{
				Path:            "C:\\ProgramData\\seekfs\\indexes\\seekfs_c.gsi",
				Entries:         100,
				Volume:          "C:",
				State:           "ready",
				JournalID:       123,
				Checkpoint:      456,
				Memory:          &residentMemoryInfo{Records: 100},
				LastPersistError: "boom",
			},
		},
		Health: "ok",
	}
	san := sanitizeRemoteResponse(resp)

	// Physical server internals must be stripped.
	if san.PID != 0 || san.Executable != "" || san.ExecutableHash != "" || san.PipeName != "" || san.ProcessMode != "" {
		t.Errorf("physical identity not stripped: %+v", san)
	}
	if san.Runtime != nil {
		t.Error("runtime memory snapshot must be stripped remotely")
	}
	if san.BlocksDecoded != 0 || san.ScalarDriver != "" || san.ComponentBounds != "" {
		t.Error("detailed trace internals must be stripped remotely")
	}
	if san.Terms != nil || san.Declines != nil {
		t.Error("trace terms/declines must be stripped remotely")
	}
	if len(san.DBs) != 1 {
		t.Fatalf("DBs count = %d, want 1", len(san.DBs))
	}
	d := san.DBs[0]
	if d.Path != "" {
		t.Errorf("db path not stripped: %q", d.Path)
	}
	if d.JournalID != 0 || d.Checkpoint != 0 {
		t.Error("journal id/checkpoint must be stripped")
	}
	if d.Memory != nil {
		t.Error("db memory must be stripped")
	}
	if d.LastPersistError != "" {
		t.Error("db persist error detail must be stripped")
	}
	// Public data preserved.
	if d.Volume != "C:" || d.State != "ready" || d.Entries != 100 {
		t.Errorf("public db fields lost: %+v", d)
	}
	if san.Count != 3 || san.OK != true || len(san.Results) != 1 || len(san.Rows) != 1 {
		t.Errorf("public result fields lost: %+v", san)
	}
	if san.Health != "ok" {
		t.Errorf("health lost: %+v", san)
	}
	// Public version/commit preserved (harmless, non-sensitive).
	if san.Version != "1.6.0" || san.Commit != "abc" {
		t.Errorf("version/commit should be preserved: %+v", san)
	}
}

func TestReadRemoteFramePayload(t *testing.T) {
	msg := `{"type":"request","id":7,"v":1,"payload":{"command":"search"}}`
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(msg)))
	buf.Write(hdr[:])
	buf.WriteString(msg)
	r := bufio.NewReader(&buf)
	got, err := readRemoteFramePayload(r)
	if err != nil {
		t.Fatalf("readRemoteFramePayload: %v", err)
	}
	if string(got) != msg {
		t.Errorf("payload mismatch:\n got %s\nwant %s", got, msg)
	}

	// EOF at boundary returns io.EOF cleanly.
	if _, err := readRemoteFramePayload(bufio.NewReader(&buf)); err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
}

func TestReadRemoteFramePayloadTooLarge(t *testing.T) {
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], remoteMaxFrameBytes+1)
	buf.Write(hdr[:])
	r := bufio.NewReader(&buf)
	if _, err := readRemoteFramePayload(r); err == nil {
		t.Fatal("expected error for oversized frame")
	}
}

func TestClampRemoteDeadline(t *testing.T) {
	now := time.Now().UnixNano()
	// Zero/non-positive -> server default (in the future).
	if got := clampRemoteDeadline(0); got <= now {
		t.Errorf("clampRemoteDeadline(0) = %d, want future default", got)
	}
	// Huge client deadline -> capped at server max.
	farFuture := now + int64(24*time.Hour)
	max := now + int64(serviceQueryTimeout)
	if got := clampRemoteDeadline(farFuture); got > max {
		t.Errorf("clampRemoteDeadline(huge) = %d, want capped at %d", got, max)
	}
	// A small reasonable deadline is preserved.
	small := now + int64(time.Second)
	if got := clampRemoteDeadline(small); got != small {
		t.Errorf("clampRemoteDeadline(small) = %d, want %d", got, small)
	}
}

func TestRemoteLoopbackServerRoundTrip(t *testing.T) {
	s := &goSearchService{}
	rs := newRemoteLoopbackServer(s, "127.0.0.1:0")
	if err := rs.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer rs.close()
	addr := rs.ln.Addr().String()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send a hello frame and read the response.
	req := remoteFrame{Type: remoteFrameHello, ID: 1, V: remoteProtocolVersion}
	writeFrame(t, conn, req)
	resp := readFrame(t, conn)
	if resp.Type != remoteFrameResponse || resp.ID != 1 {
		t.Fatalf("hello response = %+v", resp)
	}
	var hello remoteHello
	if err := json.Unmarshal(resp.Payload, &hello); err != nil {
		t.Fatalf("hello payload: %v", err)
	}
	if hello.Proto != remoteProtocolVersion || hello.Mode != "loopback" {
		t.Errorf("hello = %+v", hello)
	}
}

func TestRemoteLoopbackServerRejectsNonLoopback(t *testing.T) {
	s := &goSearchService{}
	rs := newRemoteLoopbackServer(s, "0.0.0.0:0")
	if err := rs.start(); err == nil {
		rs.close()
		t.Fatal("expected non-loopback bind to be rejected")
	}
}

func writeFrame(t *testing.T, w io.Writer, f remoteFrame) {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
}

func readFrame(t *testing.T, r io.Reader) remoteFrame {
	t.Helper()
	br := bufio.NewReader(r)
	payload, err := readRemoteFramePayload(br)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var f remoteFrame
	if err := json.Unmarshal(payload, &f); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	return f
}

func TestRemoteLoopbackSearchDeniedWhenNoIndexes(t *testing.T) {
	// A search request against a service with no loaded indexes must return a
	// sanitized error (OK=false), not a server-internal panic or leak.
	s := &goSearchService{}
	rs := newRemoteLoopbackServer(s, "127.0.0.1:0")
	if err := rs.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer rs.close()
	conn, err := net.Dial("tcp", rs.ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := remoteFrame{Type: remoteFrameRequest, ID: 5, V: remoteProtocolVersion, Payload: mustJSON(serviceRequest{Command: "search", Query: "anything"})}
	writeFrame(t, conn, req)
	resp := readFrame(t, conn)
	if resp.ID != 5 || resp.Type != remoteFrameResponse {
		t.Fatalf("response = %+v", resp)
	}
	var sresp serviceResponse
	if err := json.Unmarshal(resp.Payload, &sresp); err != nil {
		t.Fatalf("response payload: %v", err)
	}
	if sresp.OK {
		t.Error("expected OK=false for service with no indexes")
	}
	if sresp.Executable != "" || sresp.PipeName != "" || sresp.PID != 0 {
		t.Errorf("response not sanitized: %+v", sresp)
	}
	if sresp.Message == "" {
		t.Error("expected a helpful message")
	}
}

func TestRemoteWatchDeltaDenied(t *testing.T) {
	// watch-delta is not in the read-only remote allowlist; it must be denied
	// by the capability gate.
	if serviceCommandAllowed("watch-delta", serviceCapabilities{ReadOnly: true, Remote: true}) {
		t.Error("watch-delta must be denied for remote read-only callers")
	}
	if !serviceCommandAllowed("watch-delta", serviceCapabilities{ReadOnly: true}) {
		t.Error("watch-delta must stay allowed for local read-only callers")
	}
}

func TestSanitizeStripsQueryLogSensitive(t *testing.T) {
	// Ensure sanitization is idempotent and stable.
	resp := serviceResponse{OK: true, Count: 1, Message: "all good", Results: []string{`\\server\share\f.txt`}}
	a := sanitizeRemoteResponse(resp)
	b := sanitizeRemoteResponse(a)
	if !strings.EqualFold(strings.Join(a.Results, ""), strings.Join(b.Results, "")) {
		t.Errorf("sanitize not idempotent: %+v vs %+v", a, b)
	}
}
