package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/coding/extension/host/subprocess"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

type completionDelivery struct {
	Message struct {
		CustomType string `json:"customType"`
		Content    string `json:"content"`
		Display    bool   `json:"display"`
		Details    struct {
			ChildID    string `json:"childId"`
			Generation int    `json:"generation"`
		} `json:"details"`
	} `json:"message"`
	Options sdk.SendMessageOptions `json:"options"`
	Error   json.RawMessage        `json:"error,omitempty"`
}

func forwardFrames(dst io.Writer, src io.Reader, observe func(subprocess.Envelope) error) error {
	for {
		var header [4]byte
		if _, err := io.ReadFull(src, header[:]); err != nil {
			return err
		}
		size := binary.BigEndian.Uint32(header[:])
		if size == 0 || size > subprocess.MaxFrameSize {
			return fmt.Errorf("invalid extension frame size %d", size)
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(src, data); err != nil {
			return err
		}
		var envelope subprocess.Envelope
		if err := json.Unmarshal(data, &envelope); err != nil {
			return err
		}
		if err := observe(envelope); err != nil {
			return err
		}
		if _, err := io.Copy(dst, io.MultiReader(bytes.NewReader(header[:]), bytes.NewReader(data))); err != nil {
			return err
		}
	}
}

func proxyCompletionExtension(target, directory string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	socketDir, err := os.MkdirTemp("", "litter-completion-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(socketDir)
	socket := filepath.Join(socketDir, "extension.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := listener.(*net.UnixListener).SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, target, os.Args[1:]...)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "PIG_EXT_SOCKET=") && !strings.HasPrefix(value, "LITTER_FIXTURE_PROXY_TARGET=") {
			command.Env = append(command.Env, value)
		}
	}
	command.Env = append(command.Env, "PIG_EXT_SOCKET="+socket)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		return err
	}
	defer func() { _ = command.Process.Kill() }()
	child, err := listener.Accept()
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return err
	}
	defer child.Close()
	host, err := net.DialTimeout("unix", os.Getenv("PIG_EXT_SOCKET"), 10*time.Second)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return err
	}
	defer host.Close()
	go func() {
		<-ctx.Done()
		_ = host.Close()
		_ = child.Close()
	}()
	var mu sync.Mutex
	pending := map[string]completionDelivery{}
	forwarded := make(chan error, 2)
	go func() {
		forwarded <- forwardFrames(host, child, func(envelope subprocess.Envelope) error {
			if envelope.Type != subprocess.MsgCall || envelope.Call == nil || envelope.Call.Method != "sendMessage" {
				return nil
			}
			var delivery completionDelivery
			if err := json.Unmarshal(envelope.Call.Args, &delivery); err != nil {
				return err
			}
			if delivery.Message.CustomType == "litter_completion" {
				mu.Lock()
				pending[envelope.ID] = delivery
				mu.Unlock()
			}
			return nil
		})
	}()
	go func() {
		forwarded <- forwardFrames(child, host, func(envelope subprocess.Envelope) error {
			if envelope.Type != subprocess.MsgCallResult || envelope.CallResult == nil {
				return nil
			}
			mu.Lock()
			delivery, found := pending[envelope.ID]
			delete(pending, envelope.ID)
			mu.Unlock()
			if !found {
				return nil
			}
			if envelope.CallResult.Error != nil {
				delivery.Error, _ = json.Marshal(envelope.CallResult.Error)
			}
			return writeJSON(filepath.Join(directory, completionDeliveryFile(ref{delivery.Message.Details.ChildID, delivery.Message.Details.Generation})), delivery)
		})
	}()
	err = <-forwarded
	_ = host.Close()
	_ = child.Close()
	_ = <-forwarded
	if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		_ = command.Process.Kill()
		_ = command.Wait()
		return err
	}
	return command.Wait()
}

func completionDeliveryFile(run ref) string {
	return fmt.Sprintf("completion-%s-%d.json", run.ID, run.Generation)
}
