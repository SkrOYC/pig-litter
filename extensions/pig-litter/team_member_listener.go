package piglitter

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type memberControlServer struct {
	queue     *memberControlQueue
	directory string
	path      string
	dirInfo   os.FileInfo
	sockInfo  os.FileInfo
	server    *http.Server
	done      chan error
	once      sync.Once
	err       error
}

func startMemberControlServer(ctx context.Context, directory string, queue *memberControlQueue) (*memberControlServer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if queue == nil {
		return nil, reject("unavailable", "member control queue is unavailable")
	}
	if err := queue.valid(); err != nil {
		return nil, err
	}
	actual, err := canonicalTeamDirectory(directory)
	if err != nil || actual != directory {
		return nil, reject("unavailable", "member control directory is not canonical")
	}
	dirInfo, err := os.Lstat(directory)
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode().Perm()&0o077 != 0 {
		return nil, reject("unavailable", "member control directory must be private")
	}
	path := filepath.Join(directory, "member-control.sock")
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return nil, reject("unavailable", "member control socket path is already in use")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("create member control socket: %w", err)
	}
	listener.SetUnlinkOnClose(false)
	sockInfo, err := os.Lstat(path)
	currentDir, dirErr := os.Lstat(directory)
	if err != nil || dirErr != nil || sockInfo.Mode()&os.ModeSocket == 0 || !os.SameFile(dirInfo, currentDir) {
		_ = listener.Close()
		return nil, reject("unavailable", "member control socket identity changed during startup")
	}
	cleanup := func(primary error) error {
		return errors.Join(primary, listener.Close(), removeMemberControlSocket(directory, path, dirInfo, sockInfo))
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, cleanup(err)
	}
	current, err := os.Lstat(path)
	currentDir, dirErr = os.Lstat(directory)
	if err != nil || dirErr != nil || current.Mode()&os.ModeSocket == 0 || current.Mode().Perm() != 0o600 || !os.SameFile(sockInfo, current) || !os.SameFile(dirInfo, currentDir) {
		return nil, cleanup(reject("unavailable", "member control socket identity changed during startup"))
	}
	server := &memberControlServer{queue: queue, directory: directory, path: path, dirInfo: dirInfo, sockInfo: sockInfo, done: make(chan error, 1)}
	server.server = &http.Server{Handler: queue, ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 16 * 1024}
	go func() {
		err := server.server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		} else {
			queue.Close()
		}
		server.done <- err
	}()
	context.AfterFunc(ctx, func() { _ = server.Close() })
	return server, nil
}

func (server *memberControlServer) Close() error {
	server.once.Do(func() {
		server.queue.Close()
		server.err = server.server.Close()
		server.err = errors.Join(server.err, <-server.done, removeMemberControlSocket(server.directory, server.path, server.dirInfo, server.sockInfo))
	})
	return server.err
}

func removeMemberControlSocket(directory, path string, dirInfo, sockInfo os.FileInfo) error {
	currentDir, err := os.Lstat(directory)
	if err != nil || !currentDir.IsDir() || !os.SameFile(currentDir, dirInfo) {
		return errors.New("member control directory identity changed; refusing cleanup")
	}
	socket, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || socket.Mode()&os.ModeSocket == 0 || !os.SameFile(socket, sockInfo) {
		return errors.New("member control socket identity changed; refusing cleanup")
	}
	return os.Remove(path)
}
