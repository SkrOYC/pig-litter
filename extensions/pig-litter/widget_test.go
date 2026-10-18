package piglitter

import (
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension/host/subprocess"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

func TestWidgetPrioritizesOlderActiveChildrenWithoutChangingInput(t *testing.T) {
	children := []Snapshot{
		{Ref: Ref{ID: "setup"}, Name: "setup", State: Completed},
		{Ref: Ref{ID: "builder"}, Name: "builder", State: Running},
		{Ref: Ref{ID: "reviewer"}, Name: "reviewer", State: Failed},
		{Ref: Ref{ID: "finisher"}, Name: "finisher", State: Stopping},
		{Ref: Ref{ID: "newer"}, Name: "newer", State: Completed},
	}
	before := append([]Snapshot(nil), children...)
	want := []string{"Litter 2 active 1 error 0 paused 2 settled", "builder running", "finisher stopping", "/pig-litter | +3 more"}
	if got := widgetLines(children, 100); !reflect.DeepEqual(got, want) {
		t.Fatalf("widget = %q; want %q", got, want)
	}
	if !reflect.DeepEqual(children, before) {
		t.Fatal("widget changed retained snapshots")
	}
}

func TestWidgetUsesStableRetainedOrderAfterActiveChildren(t *testing.T) {
	children := []Snapshot{{Name: "oldest", State: Paused}, {Name: "failed", State: Failed}, {Name: "live", State: Starting}, {Name: "newest", State: Completed}}
	want := []string{"Litter 1 active 1 error 1 paused 1 settled", "live starting", "newest completed", "/pig-litter | +2 more"}
	if got := widgetLines(children, 100); !reflect.DeepEqual(got, want) {
		t.Fatalf("stable groups = %q", got)
	}
	want = []string{"Litter 0 active 1 error 1 paused 0 settled", "oldest paused", "failed failed", "/pig-litter"}
	if got := widgetLines(children[:2], 100); !reflect.DeepEqual(got, want) {
		t.Fatalf("settled groups = %q", got)
	}
}

func TestWidgetStateCountsAndNarrowPriority(t *testing.T) {
	var children []Snapshot
	for i, state := range []State{Starting, Running, Stopping, Pausing, Paused, Completed, Partial, Failed, Stopped, Unavailable} {
		children = append(children, Snapshot{Name: fmt.Sprintf("child-%d", i), State: state})
	}
	wide := widgetLines(children, 100)
	if wide[0] != "Litter 4 active 3 errors 1 paused 2 settled" || wide[3] != "/pig-litter | +8 more" {
		t.Fatalf("counts = %q", wide)
	}
	narrow := widgetLines(children, 32)
	if narrow[0] != "Litter 4 live 3 err 1 paused" || narrow[3] != "/pig-litter | +8 more" {
		t.Fatalf("narrow counts = %q", narrow)
	}
	if len(narrow) != 4 {
		t.Fatalf("widget has %d rows", len(narrow))
	}
}

func TestWidgetPreservesUnnamedLabelAndReservesStatus(t *testing.T) {
	id := ChildID("01995bf7-5dd4-7000-8000-000082ad1f00")
	children := []Snapshot{{Ref: Ref{ID: id}, Name: string(id), DisplayLabel: "#82ad1f00", Type: "worker", State: Running}, {Name: strings.Repeat("長👩🏽‍💻e\u0301", 20), State: Unavailable}}
	wide := widgetLines(children, 100)
	if wide[1] != "#82ad1f00 worker running" {
		t.Fatalf("unnamed = %q", wide)
	}
	for _, width := range []int{32, 16, 12, 8, 6, 3} {
		rows := widgetLines(children, width)
		assertSafeRows(t, rows, max(0, width-2))
		if width >= 16 && (!strings.HasSuffix(rows[1], " running") || !strings.HasSuffix(rows[2], " unavailable")) {
			t.Fatalf("status hidden at %d: %q", width, rows)
		}
	}
	long := []Snapshot{{Name: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ", State: Running}}
	if got := widgetLines(long, 100); got[1] != "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ running" {
		t.Fatalf("wide row still clipped = %q", got)
	}
}

func TestWidgetEmptyMinimumWidthsAndUnsafeText(t *testing.T) {
	if got := widgetLines(nil, 32); !reflect.DeepEqual(got, []string{"Litter 0 children", "/pig-litter"}) {
		t.Fatalf("empty = %q", got)
	}
	for _, width := range []int{-1, 0, 1, 2} {
		if got := widgetLines(nil, width); len(got) != 0 {
			t.Fatalf("width %d = %q", width, got)
		}
	}
	children := []Snapshot{{Name: "unsafe\x1b[2J\x1b]0;hostile\a\u202e\x00\nname", State: Running}, {Ref: Ref{ID: "raw"}, Name: "raw", Type: "x\x1bPpayload\x1b\\y", State: Failed}}
	want := []string{"Litter 1 active 1 error 0 paused 0 settled", "unsafe name running", "raw xy failed", "/pig-litter"}
	if got := widgetLines(children, 100); !reflect.DeepEqual(got, want) {
		t.Fatalf("unsafe = %q", got)
	}
	for _, width := range []int{100, 32, 8, 3} {
		assertSafeRows(t, widgetLines(children, width), width-2)
	}
}

func TestWidgetLargeRetainedHistory(t *testing.T) {
	children := make([]Snapshot, 20000)
	for i := range children {
		children[i] = Snapshot{Name: fmt.Sprintf("finished-%d", i), State: Completed}
	}
	children[0] = Snapshot{Name: "old-active", State: Running}
	children[15000] = Snapshot{Name: "other-active", State: Pausing}
	want := []string{"Litter 2 active 0 errors 0 paused 19998 settled", "old-active running", "other-active pausing", "/pig-litter | +19998 more"}
	if got := widgetLines(children, 100); !reflect.DeepEqual(got, want) {
		t.Fatalf("large retained history = %q", got)
	}
}

func BenchmarkWidgetLargeRetainedHistory(b *testing.B) {
	children := make([]Snapshot, 20000)
	for i := range children {
		children[i] = Snapshot{Name: "retained", State: Completed}
	}
	children[0].State = Running
	children[15000].State = Pausing
	b.ReportAllocs()
	for b.Loop() {
		widgetLines(children, 32)
	}
}

func TestWidgetWidthSubscriptionAndRetirementClear(t *testing.T) {
	host, peer := net.Pipe()
	defer host.Close()
	defer peer.Close()
	if err := host.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, defaultConfig())
	child := testAdmission(t, tree, Ref{}, "scout", "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
	var o *owner
	ext := sdk.New("widget-test")
	ext.OnEvent("exercise", func(ctx sdk.Context, data map[string]any) (any, error) {
		if data["action"] == "start" {
			state := &extensionState{}
			o = &owner{state: state, context: ctx, tree: tree}
			state.owner = o
			if err := o.startWidget(); err != nil {
				return nil, err
			}
			o.widget()
		} else {
			o.stopWidget()
			o.widget()
		}
		return nil, nil
	})
	readFrame := func() subprocess.Envelope {
		frame := readCompletionEnvelope(t, host)
		for frame.Type == subprocess.MsgRequestState {
			frame = readCompletionEnvelope(t, host)
		}
		return frame
	}
	done := make(chan error, 1)
	go func() { done <- ext.RunWithConn(peer) }()
	if frame := readFrame(); frame.Type != subprocess.MsgRegister {
		t.Fatal(frame)
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgReady, Ready: &subprocess.ReadyPayload{Width: 100, State: &subprocess.StatePayload{HasUI: true}}})
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "start", Request: &subprocess.RequestPayload{Method: "event", Event: "exercise", HandlerID: 1, Args: json.RawMessage(`{"action":"start"}`)}})
	readRows := func() []string {
		t.Helper()
		frame := readFrame()
		if frame.Type != subprocess.MsgWidgetPush || frame.WidgetPush == nil {
			t.Fatalf("expected widget push, got %#v", frame)
		}
		return frame.WidgetPush.Lines
	}
	wide := readRows()
	if frame := readFrame(); frame.Type != subprocess.MsgResponse || frame.ID != "start" {
		t.Fatal(frame)
	}
	for _, width := range []int{32, 100} {
		writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgNotify, Notify: &subprocess.NotifyPayload{Method: "width_change", Args: json.RawMessage(fmt.Sprintf(`{"width":%d}`, width))}})
		got := readRows()
		if width == 100 && !reflect.DeepEqual(got, wide) {
			t.Fatalf("widening lost original content: %q, %q", got, wide)
		}
		if width == 32 && (!strings.HasSuffix(got[1], " starting") || !strings.Contains(got[len(got)-1], "/pig-litter")) {
			t.Fatalf("narrow = %q", got)
		}
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "stop", Request: &subprocess.RequestPayload{Method: "event", Event: "exercise", HandlerID: 1, Args: json.RawMessage(`{"action":"stop"}`)}})
	if rows := readRows(); len(rows) != 0 {
		t.Fatalf("retirement did not clear rows: %q", rows)
	}
	if frame := readFrame(); frame.Type != subprocess.MsgResponse || frame.ID != "stop" {
		t.Fatal(frame)
	}
	if o.widgetUnsubscribe != nil {
		t.Fatal("retired width subscription retained")
	}
	if got := tree.retained(); len(got) != 1 || got[0].ID != child.ID {
		t.Fatal("widget changed child lifecycle")
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgShutdown, Shutdown: &subprocess.ShutdownPayload{Reason: "complete"}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWidgetKeepsRecentTerminalResults(t *testing.T) {
	children := []Snapshot{{Name: "first", State: Completed}, {Name: "second", State: Failed}, {Name: "third", State: Paused}, {Name: "latest", State: Completed}}
	want := []string{"Litter 0 active 1 error 1 paused 2 settled", "third paused", "latest completed", "/pig-litter | +2 more"}
	if got := widgetLines(children, 100); !reflect.DeepEqual(got, want) {
		t.Fatalf("all settled = %q; want %q", got, want)
	}
	children = append(children, Snapshot{Name: "active", State: Running})
	want = []string{"Litter 1 active 1 error 1 paused 2 settled", "active running", "latest completed", "/pig-litter | +3 more"}
	if got := widgetLines(children, 100); !reflect.DeepEqual(got, want) {
		t.Fatalf("one active = %q; want %q", got, want)
	}
}

func TestCompetingRetirementJoinsBeforeReplacementPublication(t *testing.T) {
	directory := t.TempDir()
	services, err := coding.NewServices(coding.ServicesOptions{CWD: directory, AgentDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer services.Close()
	oldTree := testTree(t, defaultConfig())
	testAdmission(t, oldTree, Ref{}, "scout", "old")
	newTree := testTree(t, defaultConfig())
	testAdmission(t, newTree, Ref{}, "scout", "replacement")
	state := &extensionState{}
	host, peer := net.Pipe()
	defer host.Close()
	defer peer.Close()
	if err := host.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var previous *owner
	ext := sdk.New("overlapping-widget-owners")
	ext.OnEvent("exercise", func(ctx sdk.Context, _ map[string]any) (any, error) {
		previous = &owner{state: state, context: ctx, tree: oldTree, services: services}
		state.owner = previous
		if err := previous.startWidget(); err != nil {
			return nil, err
		}
		previous.widget()
		return nil, nil
	})
	readFrame := func() subprocess.Envelope {
		frame := readCompletionEnvelope(t, host)
		for frame.Type == subprocess.MsgRequestState {
			frame = readCompletionEnvelope(t, host)
		}
		return frame
	}
	done := make(chan error, 1)
	go func() { done <- ext.RunWithConn(peer) }()
	if frame := readFrame(); frame.Type != subprocess.MsgRegister {
		t.Fatal(frame)
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgReady, Ready: &subprocess.ReadyPayload{Width: 100, State: &subprocess.StatePayload{HasUI: true}}})
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "start", Request: &subprocess.RequestPayload{Method: "event", Event: "exercise", HandlerID: 1, Args: json.RawMessage(`{}`)}})
	if frame := readFrame(); frame.Type != subprocess.MsgWidgetPush || frame.WidgetPush.Lines[1] != "old starting" {
		t.Fatal(frame)
	}
	if frame := readFrame(); frame.Type != subprocess.MsgResponse || frame.ID != "start" {
		t.Fatal(frame)
	}
	detached, allowClear := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(allowClear) }) }
	defer release()
	unsubscribe := previous.widgetUnsubscribe
	previous.widgetUnsubscribe = func() { close(detached); <-allowClear; unsubscribe() }
	retired := make(chan struct{})
	go func() { state.retire(); close(retired) }()
	<-detached
	state.mu.Lock()
	retirement := state.retiring
	state.mu.Unlock()
	replacementReady, replaced := make(chan struct{}), make(chan struct{})
	go func() {
		state.retire()
		close(replacementReady)
		next := &owner{state: state, context: previous.context, tree: newTree}
		state.mu.Lock()
		next.epoch = state.epoch
		state.owner = next
		state.mu.Unlock()
		next.widget()
		close(replaced)
	}()
	select {
	case <-replacementReady:
		t.Fatal("competing retirement returned before the old owner joined")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	if frame := readFrame(); frame.Type != subprocess.MsgWidgetPush || len(frame.WidgetPush.Lines) != 0 {
		t.Fatalf("old clear must precede replacement: %#v", frame)
	}
	if frame := readFrame(); frame.Type != subprocess.MsgWidgetPush || frame.WidgetPush.Lines[1] != "replacement starting" {
		t.Fatalf("replacement publication = %#v", frame)
	}
	select {
	case <-oldTree.closedDone:
	default:
		t.Fatal("new owner published before old Tree closure")
	}
	select {
	case <-retirement:
	default:
		t.Fatal("new owner published before full owner retirement")
	}
	<-replaced
	<-retired
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgShutdown, Shutdown: &subprocess.ShutdownPayload{Reason: "complete"}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
