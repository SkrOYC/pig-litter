package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
)

type collaborationMessageObservation struct {
	Stage            string `json:"stage"`
	OffsetNS         int64  `json:"offset_ns"`
	Interval         string `json:"interval,omitempty"`
	DurationNS       int64  `json:"duration_ns,omitempty"`
	MessageID        string `json:"message_id"`
	FileExists       bool   `json:"file_exists"`
	DiskCopies       int    `json:"disk_copies"`
	MemoryCopies     int    `json:"memory_copies"`
	MessageEndEvents int32  `json:"message_end_events"`
	ProviderRequests int    `json:"provider_requests"`
	Error            string `json:"error,omitempty"`
}

type collaborationMessagePhase struct {
	Name                string                            `json:"name"`
	SessionID           string                            `json:"session_id,omitempty"`
	Observations        []collaborationMessageObservation `json:"observations"`
	Requests            []json.RawMessage                 `json:"provider_requests"`
	RequestsWithMessage int                               `json:"requests_with_message"`
	Error               string                            `json:"error,omitempty"`
}

type collaborationMessageEvidence struct {
	Schema     int                         `json:"schema"`
	Purpose    string                      `json:"purpose"`
	Status     string                      `json:"status"`
	Mode       string                      `json:"mode"`
	ProcessID  int                         `json:"process_id"`
	StartedUTC string                      `json:"started_utc"`
	ElapsedNS  int64                       `json:"elapsed_ns"`
	Phases     []collaborationMessagePhase `json:"phases"`
	Error      string                      `json:"error,omitempty"`
}

type collaborationMessageFixture struct {
	root         string
	server       *httptest.Server
	services     *coding.AgentSessionServices
	runtime      *coding.Runtime
	session      *coding.Session
	start        time.Time
	holdAt       int
	holdReached  chan struct{}
	release      chan struct{}
	releaseOnce  sync.Once
	mu           sync.Mutex
	requests     []json.RawMessage
	observations []collaborationMessageObservation
	messageEnds  atomic.Int32
	eventsDone   chan struct{}
}

func collaborationProbePrepare(root string) error {
	if !filepath.IsAbs(root) {
		return errors.New("probe root must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(root, "result.json")); err == nil {
		return errors.New("probe result already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	marker, err := os.OpenFile(filepath.Join(root, ".probe-started"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("reserve probe root: %w", err)
	}
	_, writeErr := fmt.Fprintf(marker, "pid=%d\nstarted_utc=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
	return errors.Join(writeErr, marker.Close())
}

func collaborationProbeWriteJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(data, '\n'))
	return errors.Join(writeErr, file.Close())
}

func collaborationMessageFixtureNew(root string, holdAt int) (fixture *collaborationMessageFixture, resultErr error) {
	fixture = &collaborationMessageFixture{root: root, start: time.Now(), holdAt: holdAt, holdReached: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, fixture.close())
		}
	}()
	if err := os.MkdirAll(filepath.Join(root, "agent"), 0o700); err != nil {
		return fixture, err
	}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(io.LimitReader(request.Body, 2<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fixture.mu.Lock()
		fixture.requests = append(fixture.requests, append(json.RawMessage(nil), body...))
		count := len(fixture.requests)
		fixture.mu.Unlock()
		if count == fixture.holdAt {
			close(fixture.holdReached)
			select {
			case <-fixture.release:
			case <-request.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"p02-message\",\"object\":\"chat.completion.chunk\",\"model\":\"probe\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"fixture answer\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"p02-message\",\"object\":\"chat.completion.chunk\",\"model\":\"probe\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	agentDir := filepath.Join(root, "agent")
	models := fmt.Sprintf(`{"providers":{"fixture":{"api":"openai-completions","baseUrl":%q,"apiKey":"local-fixture-only","models":[{"id":"probe","name":"probe","contextWindow":32000,"maxTokens":1024}]}}}`, fixture.server.URL+"/v1")
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(models), 0o600); err != nil {
		return fixture, err
	}
	trusted := true
	var err error
	fixture.services, err = coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: root, AgentDir: agentDir, ProjectTrusted: &trusted})
	if err != nil {
		return fixture, err
	}
	fixture.runtime, err = coding.NewRuntime(coding.RuntimeOptions{Services: fixture.services})
	if err != nil {
		return fixture, err
	}
	fixture.session, err = fixture.runtime.New(coding.SessionStartOptions{SessionDir: filepath.Join(root, "sessions"), Model: fixture.services.ModelRuntime().GetModel("fixture", "probe"), ResourceLoader: coding.NoResources, AllowedTools: map[string]struct{}{}})
	if err != nil {
		return fixture, err
	}
	fixture.session.Subscribe(func(event agent.AgentEvent) {
		if end, ok := event.(agent.MessageEndEvent); ok && end.Message.Custom != nil {
			if value, _ := end.Message.Custom["customType"].(string); value == "p02_peer" {
				fixture.messageEnds.Add(1)
			}
		}
	})
	fixture.eventsDone = make(chan struct{})
	go func() {
		defer close(fixture.eventsDone)
		for event := range fixture.session.Events() {
			coding.AcknowledgeEvent(event)
		}
	}()
	return fixture, nil
}

func (fixture *collaborationMessageFixture) close() error {
	fixture.releaseOnce.Do(func() { close(fixture.release) })
	var errs []error
	if fixture.session != nil {
		errs = append(errs, fixture.session.Close())
		if fixture.eventsDone != nil {
			<-fixture.eventsDone
		}
	}
	if fixture.runtime != nil {
		errs = append(errs, fixture.runtime.Close())
	}
	if fixture.services != nil {
		fixture.services.Close()
	}
	if fixture.server != nil {
		fixture.server.Close()
	}
	return errors.Join(errs...)
}

func collaborationCountMessage(data []byte, marker string, lines bool) (int, error) {
	entries := []json.RawMessage{}
	if lines {
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		scanner.Buffer(make([]byte, 4096), 8<<20)
		for scanner.Scan() {
			entries = append(entries, append(json.RawMessage(nil), scanner.Bytes()...))
		}
		if err := scanner.Err(); err != nil {
			return 0, err
		}
	} else if err := json.Unmarshal(data, &entries); err != nil {
		return 0, err
	}
	count := 0
	for _, raw := range entries {
		var entry struct {
			Type       string `json:"type"`
			CustomType string `json:"customType"`
			Details    struct {
				MessageID string `json:"message_id"`
			} `json:"details"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return 0, err
		}
		if entry.Type == "custom_message" && entry.CustomType == "p02_peer" && entry.Details.MessageID == marker {
			count++
		}
	}
	return count, nil
}

func (fixture *collaborationMessageFixture) observe(stage, id string, sendErr error, interval string, duration time.Duration) (collaborationMessageObservation, error) {
	observation := collaborationMessageObservation{Stage: stage, OffsetNS: time.Since(fixture.start).Nanoseconds(), Interval: interval, DurationNS: duration.Nanoseconds(), MessageID: id, MessageEndEvents: fixture.messageEnds.Load()}
	if sendErr != nil {
		observation.Error = sendErr.Error()
	}
	fixture.mu.Lock()
	observation.ProviderRequests = len(fixture.requests)
	fixture.mu.Unlock()
	entries, err := json.Marshal(fixture.session.SessionManager().GetEntries())
	if err != nil {
		return observation, err
	}
	observation.MemoryCopies, err = collaborationCountMessage(entries, id, false)
	if err != nil {
		return observation, err
	}
	path := fixture.session.SessionManager().GetSessionFile()
	if path == nil {
		return observation, errors.New("session has no selected persistence path")
	}
	data, err := os.ReadFile(*path)
	if err == nil {
		observation.FileExists = true
		observation.DiskCopies, err = collaborationCountMessage(data, id, true)
		if writeErr := os.WriteFile(filepath.Join(fixture.root, stage+"-session.jsonl"), data, 0o600); writeErr != nil {
			return observation, writeErr
		}
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	fixture.mu.Lock()
	fixture.observations = append(fixture.observations, observation)
	fixture.mu.Unlock()
	return observation, err
}

func collaborationPeerMessage(id string) extension.CustomMessageRef {
	return extension.CustomMessageRef{CustomType: "p02_peer", Content: "peer message " + id, Display: false, Details: map[string]any{"message_id": id}}
}

func collaborationRunHostMessageProbe(ctx context.Context, root, mode string) (evidence collaborationMessageEvidence, resultErr error) {
	started := time.Now()
	evidence = collaborationMessageEvidence{Schema: 1, Purpose: "provisional host research; scripted provider and product parity remain unverified", Status: "inconclusive", Mode: mode, ProcessID: os.Getpid(), StartedUTC: started.UTC().Format(time.RFC3339Nano), Phases: []collaborationMessagePhase{}}
	if err := collaborationProbePrepare(root); err != nil {
		return evidence, err
	}
	defer func() {
		evidence.ElapsedNS = time.Since(started).Nanoseconds()
		if resultErr != nil {
			evidence.Status = "inconclusive"
			evidence.Error = resultErr.Error()
		}
		resultErr = errors.Join(resultErr, collaborationProbeWriteJSON(filepath.Join(root, "result.json"), evidence))
	}()
	var cases []string
	switch mode {
	case "idle":
		cases = []string{"idle-persistence", "idle-triggered"}
	case "busy":
		cases = []string{"busy-persistence", "busy-steering"}
	default:
		return evidence, fmt.Errorf("unknown message probe mode %q", mode)
	}
	for _, name := range cases {
		phase, err := collaborationRunMessageCase(ctx, filepath.Join(root, name), name)
		evidence.Phases = append(evidence.Phases, phase)
		if err != nil {
			return evidence, fmt.Errorf("%s: %w", name, err)
		}
	}
	evidence.Status = "observed"
	return evidence, nil
}

func collaborationRunMessageCase(ctx context.Context, root, name string) (phase collaborationMessagePhase, resultErr error) {
	phase = collaborationMessagePhase{Name: name}
	holdAt := 0
	if strings.HasPrefix(name, "busy-") {
		holdAt = 2
	}
	fixture, err := collaborationMessageFixtureNew(root, holdAt)
	if err != nil {
		phase.Error = err.Error()
		return phase, err
	}
	phase.SessionID = fixture.session.SessionManager().ID()
	defer func() {
		resultErr = errors.Join(resultErr, fixture.close())
		fixture.mu.Lock()
		phase.Observations = append([]collaborationMessageObservation(nil), fixture.observations...)
		phase.Requests = append([]json.RawMessage(nil), fixture.requests...)
		fixture.mu.Unlock()
		messageID := map[string]string{"idle-triggered": "idle-triggered", "busy-steering": "busy-steering"}[name]
		if messageID != "" {
			for _, request := range phase.Requests {
				if strings.Contains(string(request), "peer message "+messageID) {
					phase.RequestsWithMessage++
				}
			}
		}
		if resultErr != nil {
			phase.Error = resultErr.Error()
		}
		resultErr = errors.Join(resultErr, collaborationProbeWriteJSON(filepath.Join(root, "observations.json"), phase))
	}()
	trigger := false
	if name == "idle-persistence" {
		start := time.Now()
		sendErr := fixture.session.SendCustomMessage(ctx, collaborationPeerMessage("idle-fresh"), &extension.SendMessageOptions{TriggerTurn: &trigger})
		fresh, observeErr := fixture.observe("fresh-idle-return", "idle-fresh", sendErr, "send_call", time.Since(start))
		if sendErr != nil || observeErr != nil || fresh.FileExists || fresh.MemoryCopies != 1 || fresh.DiskCopies != 0 || fresh.MessageEndEvents != 1 || fresh.ProviderRequests != 0 {
			return phase, fmt.Errorf("fresh idle send = %#v, send=%v observe=%v", fresh, sendErr, observeErr)
		}
		if _, err := fixture.session.Send(ctx, "bootstrap conversation"); err != nil {
			return phase, err
		}
		flushed, observeErr := fixture.observe("after-first-conversation", "idle-fresh", nil, "", 0)
		if observeErr != nil || !flushed.FileExists || flushed.DiskCopies != 1 || flushed.ProviderRequests != 1 {
			return phase, fmt.Errorf("first conversation = %#v, observe=%v", flushed, observeErr)
		}
		start = time.Now()
		sendErr = fixture.session.SendCustomMessage(ctx, collaborationPeerMessage("idle-established"), &extension.SendMessageOptions{TriggerTurn: &trigger})
		established, observeErr := fixture.observe("established-idle-return", "idle-established", sendErr, "send_call", time.Since(start))
		if sendErr != nil || observeErr != nil || established.DiskCopies != 1 || established.MessageEndEvents != 2 || established.ProviderRequests != 1 {
			return phase, fmt.Errorf("established idle send = %#v, send=%v observe=%v", established, sendErr, observeErr)
		}
		return phase, nil
	}
	if _, err := fixture.session.Send(ctx, "bootstrap conversation"); err != nil {
		return phase, err
	}
	if name == "idle-triggered" {
		trigger = true
		start := time.Now()
		sendErr := fixture.session.SendCustomMessage(ctx, collaborationPeerMessage("idle-triggered"), &extension.SendMessageOptions{TriggerTurn: &trigger})
		observation, observeErr := fixture.observe("idle-triggered-return", "idle-triggered", sendErr, "send_call", time.Since(start))
		if sendErr != nil || observeErr != nil || observation.DiskCopies != 1 || observation.MessageEndEvents != 1 || observation.ProviderRequests != 2 {
			return phase, fmt.Errorf("idle triggered send = %#v, send=%v observe=%v", observation, sendErr, observeErr)
		}
		fixture.mu.Lock()
		contains := strings.Contains(string(fixture.requests[1]), "peer message idle-triggered")
		fixture.mu.Unlock()
		if !contains {
			return phase, errors.New("triggered provider request omitted the peer message")
		}
		return phase, nil
	}
	if name != "busy-persistence" && name != "busy-steering" {
		return phase, fmt.Errorf("unknown message phase %q", name)
	}
	done := make(chan error, 1)
	go func() { _, err := fixture.session.Send(ctx, "hold the provider response"); done <- err }()
	select {
	case <-fixture.holdReached:
	case <-ctx.Done():
		return phase, fmt.Errorf("provider did not reach the busy barrier: %w", ctx.Err())
	}
	id := "busy-deferred"
	options := &extension.SendMessageOptions{TriggerTurn: &trigger}
	if name == "busy-steering" {
		id = "busy-steering"
		options = nil
	}
	start := time.Now()
	sendErr := fixture.session.SendCustomMessage(ctx, collaborationPeerMessage(id), options)
	queued, observeErr := fixture.observe(name+"-return", id, sendErr, "send_call", time.Since(start))
	if sendErr != nil || observeErr != nil || queued.DiskCopies != 0 || queued.MemoryCopies != 0 || queued.MessageEndEvents != 0 || queued.ProviderRequests != 2 {
		return phase, fmt.Errorf("busy send = %#v, send=%v observe=%v", queued, sendErr, observeErr)
	}
	start = time.Now()
	fixture.releaseOnce.Do(func() { close(fixture.release) })
	select {
	case err := <-done:
		if err != nil {
			return phase, err
		}
	case <-ctx.Done():
		return phase, fmt.Errorf("busy turn did not settle: %w", ctx.Err())
	}
	settled, observeErr := fixture.observe(name+"-settled", id, nil, "release_to_settled", time.Since(start))
	wantRequests := 2
	if name == "busy-steering" {
		wantRequests = 3
	}
	if observeErr != nil || settled.DiskCopies != 1 || settled.MessageEndEvents != 1 || settled.ProviderRequests != wantRequests {
		return phase, fmt.Errorf("busy settled = %#v, observe=%v", settled, observeErr)
	}
	if name == "busy-steering" {
		fixture.mu.Lock()
		contains := strings.Contains(string(fixture.requests[2]), "peer message busy-steering")
		fixture.mu.Unlock()
		if !contains {
			return phase, errors.New("steering provider request omitted the peer message")
		}
	}
	return phase, nil
}
