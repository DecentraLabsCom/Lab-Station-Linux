package privileged

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"
)

const SocketPath = "/run/decentralabs-lab-station-helper/helper.sock"

const (
	maxRequestBytes  = 8192
	maxResponseBytes = 4096
)

type response struct {
	OK     bool            `json:"ok"`
	Output json.RawMessage `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Call submits one allowlisted helper request over the root-owned local socket.
// connected distinguishes an unavailable helper service from a rejected request,
// so callers never retry an operation that may already have completed.
func Call(ctx context.Context, payload []byte) (connected bool, output []byte, err error) {
	if len(payload) == 0 || len(payload) > maxRequestBytes {
		return false, nil, errors.New("privileged helper request has an invalid size")
	}

	conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", SocketPath)
	if err != nil {
		return false, nil, err
	}
	connected = true
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	}

	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return true, nil, errors.New("privileged helper connection is not a Unix socket")
	}
	request := append(append([]byte(nil), payload...), '\n')
	if _, err := io.Copy(unixConn, bytes.NewReader(request)); err != nil {
		return true, nil, err
	}
	if err := unixConn.CloseWrite(); err != nil {
		return true, nil, err
	}

	encoded, err := io.ReadAll(io.LimitReader(unixConn, maxResponseBytes+1))
	if err != nil {
		return true, nil, err
	}
	if len(encoded) > maxResponseBytes {
		return true, nil, errors.New("privileged helper response exceeds its size limit")
	}
	var result response
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return true, nil, errors.New("privileged helper returned an invalid response")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return true, nil, errors.New("privileged helper returned an invalid response")
	}
	if !result.OK {
		if result.Error == "" {
			return true, nil, errors.New("privileged helper rejected the operation")
		}
		return true, nil, errors.New(result.Error)
	}
	return true, result.Output, nil
}
