package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/decentralabs/lab-station-linux/internal/privileged"
)

const maxPrivilegedRequestBytes = 8192

type socketResponse struct {
	OK     bool            `json:"ok"`
	Output json.RawMessage `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}

func servePrivilegedSocket() error {
	group, err := user.LookupGroup("labstation")
	if err != nil {
		return errors.New("labstation group is unavailable")
	}
	groupID, err := strconv.ParseUint(group.Gid, 10, 32)
	if err != nil {
		return errors.New("labstation group ID is invalid")
	}
	if err := validateSocketDirectory(filepath.Dir(privileged.SocketPath), uint32(groupID)); err != nil {
		return err
	}
	if err := removeStaleSocket(privileged.SocketPath); err != nil {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: privileged.SocketPath, Net: "unix"})
	if err != nil {
		return err
	}
	listener.SetUnlinkOnClose(true)
	defer listener.Close()
	if err := os.Chown(privileged.SocketPath, 0, int(groupID)); err != nil {
		return err
	}
	if err := os.Chmod(privileged.SocketPath, 0660); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		connection, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		handlePrivilegedConnection(connection)
	}
}

func validateSocketDirectory(path string, labstationGID uint32) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0750 {
		return errors.New("privileged helper runtime directory is unsafe")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || stat.Gid != labstationGID {
		return errors.New("privileged helper runtime directory owner is invalid")
	}
	return nil
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return errors.New("privileged helper socket path is occupied by an unsafe file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return errors.New("privileged helper socket owner is invalid")
	}
	connection, dialErr := net.DialTimeout("unix", path, time.Second)
	if dialErr == nil {
		_ = connection.Close()
		return errors.New("privileged helper socket is already active")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return nil
}

func handlePrivilegedConnection(connection *net.UnixConn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(2 * time.Minute))

	peerUID, err := unixPeerUID(connection)
	if err != nil {
		writeSocketResponse(connection, socketResponse{Error: "caller identity is unavailable"})
		return
	}
	peer, err := user.LookupId(strconv.FormatUint(uint64(peerUID), 10))
	if err != nil {
		writeSocketResponse(connection, socketResponse{Error: "caller identity is invalid"})
		return
	}
	if peer.Username != "root" && peer.Username != "labstationd" {
		writeSocketResponse(connection, socketResponse{Error: "helper caller is not authorized for this operation"})
		return
	}

	encoded, err := io.ReadAll(io.LimitReader(connection, maxPrivilegedRequestBytes+1))
	if err != nil || len(encoded) == 0 || len(encoded) > maxPrivilegedRequestBytes {
		writeSocketResponse(connection, socketResponse{Error: "invalid helper request"})
		return
	}
	request, err := decodeRequest(strings.NewReader(string(encoded)))
	if err != nil || requestOperationClass(request.Operation) != "control" || !callerCanRunOperation(peer.Username, request.Operation) {
		writeSocketResponse(connection, socketResponse{Error: "helper caller is not authorized for this operation"})
		return
	}
	output, err := executeRequest(request)
	if err != nil {
		fmt.Fprintf(os.Stderr, "privileged helper rejected operation %q for uid %d\n", request.Operation, peerUID)
		writeSocketResponse(connection, socketResponse{Error: err.Error()})
		return
	}
	writeSocketResponse(connection, socketResponse{OK: true, Output: output})
}

func unixPeerUID(connection *net.UnixConn) (uint32, error) {
	rawConnection, err := connection.SyscallConn()
	if err != nil {
		return 0, err
	}
	var credential *syscall.Ucred
	var socketErr error
	if err := rawConnection.Control(func(fd uintptr) {
		credential, socketErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if socketErr != nil || credential == nil {
		return 0, errors.New("peer credentials are unavailable")
	}
	return credential.Uid, nil
}

func writeSocketResponse(connection *net.UnixConn, response socketResponse) {
	if response.Error != "" {
		response.OK = false
	}
	_ = json.NewEncoder(connection).Encode(response)
}
