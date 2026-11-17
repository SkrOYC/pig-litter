package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
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

func collaborationProbeDirectory(t *testing.T, prefix string) string {
	t.Helper()
	base := os.Getenv("P02_PROBE_EVIDENCE_ROOT")
	if base == "" {
		return t.TempDir()
	}
	if !filepath.IsAbs(base) {
		t.Fatal("P02_PROBE_EVIDENCE_ROOT must be absolute")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, prefix+"-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("probe evidence: %s", dir)
	return dir
}

func collaborationMessageFixtureNew(t *testing.T, holdAt int) *collaborationMessageFixture {
	t.Helper()
	root := collaborationProbeDirectory(t, "message")
	t.Setenv("PIG_OFFLINE", "1")
	t.Setenv("PI_OFFLINE", "1")
	t.Setenv("HOME", filepath.Join(root, "home"))
	fixture := &collaborationMessageFixture{root: root, start: time.Now(), holdAt: holdAt, holdReached: make(chan struct{}), release: make(chan struct{}), eventsDone: make(chan struct{})}
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
	t.Cleanup(func() {
		fixture.releaseOnce.Do(func() { close(fixture.release) })
		sessionID := ""
		if fixture.session != nil {
			sessionID = fixture.session.SessionManager().ID()
			_ = fixture.session.Close()
			<-fixture.eventsDone
		}
		if fixture.runtime != nil {
			_ = fixture.runtime.Close()
		}
		if fixture.services != nil {
			fixture.services.Close()
		}
		fixture.server.Close()
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		data, _ := json.MarshalIndent(map[string]any{"started_utc": fixture.start.UTC().Format(time.RFC3339Nano), "process_id": os.Getpid(), "session_id": sessionID, "observations": fixture.observations, "requests": fixture.requests}, "", "  ")
		if err := os.WriteFile(filepath.Join(root, "observations.json"), append(data, '\n'), 0o600); err != nil {
			t.Errorf("retain message observations: %v", err)
		}
	})
	agentDir := filepath.Join(root, "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	models := fmt.Sprintf(`{"providers":{"fixture":{"api":"openai-completions","baseUrl":%q,"apiKey":"local-fixture-only","models":[{"id":"probe","name":"probe","contextWindow":32000,"maxTokens":1024}]}}}`, fixture.server.URL+"/v1")
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	trusted := true
	var err error
	fixture.services, err = coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: root, AgentDir: agentDir, ProjectTrusted: &trusted})
	if err != nil {
		t.Fatal(err)
	}
	fixture.runtime, err = coding.NewRuntime(coding.RuntimeOptions{Services: fixture.services})
	if err != nil {
		t.Fatal(err)
	}
	fixture.session, err = fixture.runtime.New(coding.SessionStartOptions{SessionDir: filepath.Join(root, "sessions"), Model: fixture.services.ModelRuntime().GetModel("fixture", "probe"), ResourceLoader: coding.NoResources, AllowedTools: map[string]struct{}{}})
	if err != nil {
		t.Fatal(err)
	}
	fixture.session.Subscribe(func(event agent.AgentEvent) {
		if end, ok := event.(agent.MessageEndEvent); ok && end.Message.Custom != nil {
			if value, _ := end.Message.Custom["customType"].(string); value == "p02_peer" {
				fixture.messageEnds.Add(1)
			}
		}
	})
	go func() {
		defer close(fixture.eventsDone)
		for event := range fixture.session.Events() {
			coding.AcknowledgeEvent(event)
		}
	}()
	return fixture
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
		return observation, fmt.Errorf("session has no selected persistence path")
	}
	data, err := os.ReadFile(*path)
	if err == nil {
		observation.FileExists = true
		observation.DiskCopies, err = collaborationCountMessage(data, id, true)
		if writeErr := os.WriteFile(filepath.Join(fixture.root, stage+"-session.jsonl"), data, 0o600); writeErr != nil {
			return observation, writeErr
		}
	} else if os.IsNotExist(err) {
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

func TestCollaborationHostIdleMessagePersistence(t *testing.T) {
	fixture := collaborationMessageFixtureNew(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	trigger := false
	id := "idle-fresh"
	start := time.Now()
	err := fixture.session.SendCustomMessage(ctx, collaborationPeerMessage(id), &extension.SendMessageOptions{TriggerTurn: &trigger})
	fresh, observeErr := fixture.observe("fresh-idle-return", id, err, "send_call", time.Since(start))
	if err != nil || observeErr != nil || fresh.FileExists || fresh.MemoryCopies != 1 || fresh.DiskCopies != 0 || fresh.MessageEndEvents != 1 || fresh.ProviderRequests != 0 {
		t.Fatalf("fresh idle send = %#v, send=%v observe=%v", fresh, err, observeErr)
	}
	if _, err := fixture.session.Send(ctx, "bootstrap conversation"); err != nil {
		t.Fatal(err)
	}
	flushed, observeErr := fixture.observe("after-first-conversation", id, nil, "", 0)
	if observeErr != nil || !flushed.FileExists || flushed.DiskCopies != 1 || flushed.ProviderRequests != 1 {
		t.Fatalf("first conversation = %#v, %v", flushed, observeErr)
	}
	id = "idle-established"
	start = time.Now()
	err = fixture.session.SendCustomMessage(ctx, collaborationPeerMessage(id), &extension.SendMessageOptions{TriggerTurn: &trigger})
	established, observeErr := fixture.observe("established-idle-return", id, err, "send_call", time.Since(start))
	if err != nil || observeErr != nil || established.DiskCopies != 1 || established.MessageEndEvents != 2 || established.ProviderRequests != 1 {
		t.Fatalf("established idle send = %#v, send=%v observe=%v", established, err, observeErr)
	}
	t.Logf("fresh idle return: file=%t memory=%d disk=%d requests=%d; after first conversation disk=%d; established idle disk=%d requests=%d", fresh.FileExists, fresh.MemoryCopies, fresh.DiskCopies, fresh.ProviderRequests, flushed.DiskCopies, established.DiskCopies, established.ProviderRequests)
}

func TestCollaborationHostBusyMessagePersistence(t *testing.T) {
	fixture := collaborationMessageFixtureNew(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := fixture.session.Send(ctx, "bootstrap conversation"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := fixture.session.Send(ctx, "hold the provider response"); done <- err }()
	select {
	case <-fixture.holdReached:
	case <-ctx.Done():
		t.Fatal("provider did not reach the busy barrier")
	}
	trigger := false
	id := "busy-deferred"
	start := time.Now()
	err := fixture.session.SendCustomMessage(ctx, collaborationPeerMessage(id), &extension.SendMessageOptions{TriggerTurn: &trigger})
	queued, observeErr := fixture.observe("busy-send-return", id, err, "send_call", time.Since(start))
	if err != nil || observeErr != nil || queued.DiskCopies != 0 || queued.MemoryCopies != 0 || queued.MessageEndEvents != 0 || queued.ProviderRequests != 2 {
		t.Fatalf("busy send = %#v, send=%v observe=%v", queued, err, observeErr)
	}
	start = time.Now()
	fixture.releaseOnce.Do(func() { close(fixture.release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	settled, observeErr := fixture.observe("busy-turn-settled", id, nil, "release_to_settled", time.Since(start))
	if observeErr != nil || settled.DiskCopies != 1 || settled.MessageEndEvents != 1 || settled.ProviderRequests != 2 {
		t.Fatalf("busy settled = %#v, %v", settled, observeErr)
	}
	t.Logf("busy return: disk=%d memory=%d requests=%d; settled disk=%d requests=%d", queued.DiskCopies, queued.MemoryCopies, queued.ProviderRequests, settled.DiskCopies, settled.ProviderRequests)
}

func TestCollaborationHostIdleTriggeredMessage(t *testing.T) {
	fixture := collaborationMessageFixtureNew(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := fixture.session.Send(ctx, "bootstrap conversation"); err != nil {
		t.Fatal(err)
	}
	trigger := true
	id := "idle-triggered"
	start := time.Now()
	err := fixture.session.SendCustomMessage(ctx, collaborationPeerMessage(id), &extension.SendMessageOptions{TriggerTurn: &trigger})
	observation, observeErr := fixture.observe("idle-triggered-return", id, err, "send_call", time.Since(start))
	if err != nil || observeErr != nil || observation.DiskCopies != 1 || observation.MessageEndEvents != 1 || observation.ProviderRequests != 2 {
		t.Fatalf("idle triggered send = %#v, send=%v observe=%v", observation, err, observeErr)
	}
	fixture.mu.Lock()
	request := append([]byte(nil), fixture.requests[1]...)
	fixture.mu.Unlock()
	if !strings.Contains(string(request), id) {
		t.Fatalf("triggered provider request did not contain the peer message: %s", request)
	}
	t.Logf("idle triggered return: disk=%d events=%d requests=%d", observation.DiskCopies, observation.MessageEndEvents, observation.ProviderRequests)
}

func TestCollaborationHostBusySteeringMessage(t *testing.T) {
	fixture := collaborationMessageFixtureNew(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := fixture.session.Send(ctx, "bootstrap conversation"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := fixture.session.Send(ctx, "hold the provider response"); done <- err }()
	select {
	case <-fixture.holdReached:
	case <-ctx.Done():
		t.Fatal("provider did not reach the busy barrier")
	}
	id := "busy-steering"
	start := time.Now()
	err := fixture.session.SendCustomMessage(ctx, collaborationPeerMessage(id), nil)
	queued, observeErr := fixture.observe("busy-steering-return", id, err, "send_call", time.Since(start))
	if err != nil || observeErr != nil || queued.DiskCopies != 0 || queued.MessageEndEvents != 0 || queued.ProviderRequests != 2 {
		t.Fatalf("busy steering return = %#v, send=%v observe=%v", queued, err, observeErr)
	}
	start = time.Now()
	fixture.releaseOnce.Do(func() { close(fixture.release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	settled, observeErr := fixture.observe("busy-steering-settled", id, nil, "release_to_settled", time.Since(start))
	if observeErr != nil || settled.DiskCopies != 1 || settled.MessageEndEvents != 1 {
		t.Fatalf("busy steering settled = %#v, %v", settled, observeErr)
	}
	fixture.mu.Lock()
	requestsWithMessage := 0
	for _, request := range fixture.requests {
		if strings.Contains(string(request), id) {
			requestsWithMessage++
		}
	}
	fixture.mu.Unlock()
	t.Logf("busy steering return: disk=%d requests=%d; settled disk=%d requests=%d providerRequestsContainingMessage=%d", queued.DiskCopies, queued.ProviderRequests, settled.DiskCopies, settled.ProviderRequests, requestsWithMessage)
}
