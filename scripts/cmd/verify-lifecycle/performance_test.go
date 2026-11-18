package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type blockedBody struct {
	reader  io.Reader
	entered chan struct{}
	release chan struct{}
}

func (body *blockedBody) Read(p []byte) (int, error) {
	select {
	case <-body.entered:
	default:
		close(body.entered)
		<-body.release
	}
	return body.reader.Read(p)
}

func (body *blockedBody) Close() error { return nil }

func TestPerformanceHandlerRetainsArrivalBeforeOutOfOrderDecoding(t *testing.T) {
	request := modelRequestWithTools("held", "read")
	request.Messages = []message{{Role: "system", Content: json.RawMessage(`"input.txt"`)}, {Role: "user", Content: json.RawMessage(`"stop-D"`)}}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	perf := &performance{}
	f := &fixture{scenario: "core", performance: perf, held: map[string]chan struct{}{}, changed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body := &blockedBody{reader: bytes.NewReader(raw), entered: make(chan struct{}), release: make(chan struct{})}
	first := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		f.serve(httptest.NewRecorder(), first)
		close(done)
	}()
	<-body.entered
	secondArrival := time.Now()
	second := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw)).WithContext(ctx)
	response := httptest.NewRecorder()
	f.serve(response, second)
	close(body.release)
	<-done
	if response.Code != http.StatusOK || f.failure != nil || perf.child.IsZero() || !perf.child.Before(secondArrival) {
		t.Fatalf("earlier handler arrival was not retained: status=%d failure=%v arrival=%v second=%v", response.Code, f.failure, perf.child, secondArrival)
	}
}

func TestPerformanceEarliestValidArrivalAndMissingEvents(t *testing.T) {
	start := time.Now()
	p := &performance{started: start, trace: residentTrace{PeakBytes: 4096}}
	p.trace.Observations = append(p.trace.Observations, residentObservationForTest())
	p.markRequest("parent", start.Add(10*time.Millisecond))
	p.markSpawn(start.Add(20 * time.Millisecond))
	var workers sync.WaitGroup
	workers.Add(1)
	laterDecoded := make(chan struct{})
	go func() {
		defer workers.Done()
		<-laterDecoded
		p.markRequest("held", start.Add(25*time.Millisecond))
	}()
	p.markRequest("held", start.Add(40*time.Millisecond))
	close(laterDecoded)
	workers.Wait()
	dir := t.TempDir()
	if err := p.publish(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "performance.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sample performanceSample
	if err := json.Unmarshal(raw, &sample); err != nil {
		t.Fatal(err)
	}
	if sample.StartupNS != 10_000_000 || sample.SpawnToFirstRequestNS != 5_000_000 || sample.PeakTreeRSSBytes != 4096 {
		t.Fatalf("unexpected recorded durations or RSS: %#v", sample)
	}
	missing := &performance{started: start, trace: p.trace}
	missing.markRequest("parent", start.Add(time.Millisecond))
	missing.markSpawn(start.Add(2 * time.Millisecond))
	missingDir := t.TempDir()
	if err := missing.publish(missingDir); err == nil {
		t.Fatal("accepted missing child arrival")
	}
	if _, err := os.Stat(filepath.Join(missingDir, "performance.json")); !os.IsNotExist(err) {
		t.Fatalf("published invalid sample: %v", err)
	}
	missing.markRequest("held", start.Add(time.Millisecond))
	if err := missing.publish(missingDir); err == nil {
		t.Fatal("accepted child arrival before spawn")
	}
}

func TestPerformanceResultPublicationRequiresMetrics(t *testing.T) {
	dir := t.TempDir()
	if err := publishFixtureResult(dir, &performance{}, map[string]bool{"passed": true}); err == nil {
		t.Fatal("accepted a passed result with missing measurements")
	}
	if _, err := os.Stat(filepath.Join(dir, "result.json")); !os.IsNotExist(err) {
		t.Fatalf("published a passed result without measurements: %v", err)
	}
	start := time.Now()
	p := &performance{started: start, trace: residentTrace{PeakBytes: 4096}}
	p.trace.Observations = append(p.trace.Observations, residentObservationForTest())
	p.markRequest("parent", start.Add(time.Millisecond))
	p.markSpawn(start.Add(2 * time.Millisecond))
	p.markRequest("held", start.Add(3*time.Millisecond))
	if err := os.Mkdir(filepath.Join(dir, "result.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := publishFixtureResult(dir, p, map[string]bool{"passed": true}); err == nil {
		t.Fatal("accepted an unwritable result")
	}
	if _, err := os.Stat(filepath.Join(dir, "performance.json")); !os.IsNotExist(err) {
		t.Fatalf("retained successful measurements without a result: %v", err)
	}
}

func TestPerformanceRejectsNonCoreBeforeStartingHost(t *testing.T) {
	if os.Getenv("PERFORMANCE_FLAG_HELPER") == "1" {
		flag.CommandLine = flag.NewFlagSet("fixture", flag.ExitOnError)
		os.Args = []string{"fixture", "--scenario", "cwd", "--measure-performance", "--pig-bin", os.Getenv("PERFORMANCE_FAKE_HOST"), "--evidence-root", os.Getenv("PERFORMANCE_EVIDENCE")}
		main()
		return
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	host := filepath.Join(dir, "host")
	if err := os.WriteFile(host, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(dir, "evidence")
	command := exec.Command(os.Args[0], "-test.run=^TestPerformanceRejectsNonCoreBeforeStartingHost$")
	command.Env = append(os.Environ(), "PERFORMANCE_FLAG_HELPER=1", "PERFORMANCE_FAKE_HOST="+host, "PERFORMANCE_EVIDENCE="+evidence, "LITTER_FIXTURE_PROXY_TARGET=")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("accepted non-core measurement: %s", output)
	}
	if _, err := os.Stat(evidence); !os.IsNotExist(err) {
		t.Fatalf("created evidence before rejecting flag: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("started host before rejecting flag: %v", err)
	}
}
