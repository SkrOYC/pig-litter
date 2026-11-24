package piglitter

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
	"github.com/gofrs/flock"
)

const (
	observerPairLimit     = 256
	observerReportLimit   = 64
	observerPendingLimit  = 8
	observerRecoveryBatch = 32
	observerJournalBytes  = 2 * 1024 * 1024
)

var observerRequestSchema = object([]string{"id", "reportTo"}, map[string]any{
	"id":       text(128),
	"reportTo": map[string]any{"type": "string", "enum": []string{"worker", "coordinator"}},
})

var observerReportID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

type observerRequest struct {
	ID       ChildID `json:"id"`
	ReportTo string  `json:"reportTo"`
}

type observerIdentity struct {
	Version     int     `json:"version"`
	Root        ChildID `json:"root"`
	SessionFile string  `json:"sessionFile"`
	Cwd         string  `json:"cwd"`
	PrivateKey  []byte  `json:"privateKey"`
}

type observerAttempt struct {
	Host            peerHostIdentity `json:"host"`
	SessionFile     string           `json:"sessionFile"`
	After           string           `json:"after"`
	Owner           string           `json:"owner"`
	WriterExecution string           `json:"writerExecution"`
	Root            bool             `json:"root"`
}

type observerMemberPin struct {
	Ref       MemberRef `json:"ref"`
	Admission string    `json:"admission"`
}

type observerReceipt struct {
	ID          string           `json:"id"`
	Text        string           `json:"text"`
	Digest      string           `json:"digest"`
	State       string           `json:"state"`
	Attempt     *observerAttempt `json:"attempt,omitempty"`
	NativeEntry string           `json:"nativeEntry,omitempty"`
	Reason      string           `json:"reason,omitempty"`
}

type observerJournal struct {
	Version              int                `json:"version"`
	ID                   string             `json:"id"`
	Root                 ChildID            `json:"root"`
	Observer             ChildID            `json:"observer"`
	ObserverExecution    string             `json:"observerExecution"`
	Observed             ChildID            `json:"observed"`
	ObservedExecution    string             `json:"observedExecution"`
	Coordinator          ChildID            `json:"coordinator"`
	CoordinatorExecution string             `json:"coordinatorExecution,omitempty"`
	Target               ChildID            `json:"target"`
	TargetFile           string             `json:"targetFile"`
	ReportTo             string             `json:"reportTo"`
	State                string             `json:"state"`
	TeamCreation         string             `json:"teamCreation,omitempty"`
	ObserverMember       *observerMemberPin `json:"observerMember,omitempty"`
	ObservedMember       *observerMemberPin `json:"observedMember,omitempty"`
	CoordinatorMember    *observerMemberPin `json:"coordinatorMember,omitempty"`
	TargetMember         *observerMemberPin `json:"targetMember,omitempty"`
	Reports              []observerReceipt  `json:"reports"`
}

type signedObserverJournal struct {
	Journal   json.RawMessage `json:"journal"`
	Signature string          `json:"signature"`
}

type observerPair struct {
	mu             sync.Mutex
	journal        observerJournal
	store          *observerStore
	observer       *run
	observed       *run
	coordinator    *run
	teamAncestor   *run
	lastDigest     observerDigest
	lastUpdate     time.Time
	digestPending  *observerDigest
	digestTimer    *time.Timer
	digestInFlight uint64
	digestSequence uint64
	digestSent     observerDigest
	digestSentAt   time.Time
}

type observerStore struct {
	mu         sync.Mutex
	dir        string
	key        ed25519.PrivateKey
	lease      *flock.Flock
	owner      string
	pairs      map[string]*observerPair
	count      int
	recovering bool
	rerun      bool
}

type observerDigest struct {
	Version      uint64         `json:"version"`
	Observed     ChildID        `json:"observed"`
	Execution    string         `json:"execution"`
	State        State          `json:"state"`
	CapturedAt   time.Time      `json:"capturedAt"`
	SourceAt     time.Time      `json:"sourceAt"`
	Availability string         `json:"availability"`
	Turns        int            `json:"turns"`
	ToolStarts   uint64         `json:"toolStarts"`
	Active       map[string]int `json:"active"`
	Clipped      bool           `json:"clipped"`
}

type observerCallTrace struct {
	mu      sync.Mutex
	partial map[int]*strings.Builder
	ready   map[string][]byte
}

func newObserverCallTrace() *observerCallTrace {
	return &observerCallTrace{partial: map[int]*strings.Builder{}, ready: map[string][]byte{}}
}

func (t *observerCallTrace) observe(event ai.AssistantMessageEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch event := event.(type) {
	case ai.ToolCallStartEvent:
		if len(t.partial) < activeToolLimit {
			t.partial[event.ContentIndex] = &strings.Builder{}
		}
	case ai.ToolCallDeltaEvent:
		if body := t.partial[event.ContentIndex]; body != nil {
			if body.Len()+len(event.Delta) > 6*4096+1024 {
				delete(t.partial, event.ContentIndex)
			} else {
				body.WriteString(event.Delta)
			}
		}
	case ai.ToolCallEndEvent:
		body := t.partial[event.ContentIndex]
		delete(t.partial, event.ContentIndex)
		if event.ToolCall.Name == "observer_report" && event.ToolCall.ID != "" && body != nil && len(t.ready) < activeToolLimit {
			t.ready[event.ToolCall.ID] = []byte(body.String())
		}
	}
}

func (t *observerCallTrace) take(id string) ([]byte, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	raw, ok := t.ready[id]
	delete(t.ready, id)
	return raw, ok
}

func newObserverPair(root, coordinator *record, coordinatorRun, observer, observed *run, reportTo string) *observerPair {
	target := coordinator.id
	if reportTo == "worker" {
		target = observed.ref.ID
	}
	coordinatorExecution := ""
	if coordinatorRun != nil {
		coordinatorExecution = coordinatorRun.noticeID
	}
	pair := &observerPair{
		journal:  observerJournal{Version: 1, ID: observer.noticeID, Root: root.id, Observer: observer.ref.ID, ObserverExecution: observer.noticeID, Observed: observed.ref.ID, ObservedExecution: observed.noticeID, Coordinator: coordinator.id, CoordinatorExecution: coordinatorExecution, Target: target, ReportTo: reportTo, State: "pending", Reports: []observerReceipt{}},
		observer: observer, observed: observed, coordinator: coordinatorRun,
	}
	pin := func(r *record) *observerMemberPin {
		if r == nil || r.team == nil {
			return nil
		}
		return &observerMemberPin{Ref: r.team.Ref, Admission: r.team.AdmissionID}
	}
	pair.journal.ObserverMember = pin(observer.owner)
	pair.journal.ObservedMember = pin(observed.owner)
	for ancestor := coordinator; ancestor != nil && ancestor != root; ancestor = ancestor.parent {
		if ancestor.team != nil {
			pair.journal.CoordinatorMember = pin(ancestor)
			pair.teamAncestor = ancestor.run
			break
		}
	}
	if reportTo == "worker" {
		pair.journal.TargetMember = pair.journal.ObservedMember
	} else if coordinator.team != nil {
		pair.journal.TargetMember = pin(coordinator)
	}
	return pair
}

func (p *observerPair) presentation() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	pending, observed, undeliverable := 0, 0, 0
	receipts := make([]map[string]string, 0, len(p.journal.Reports))
	for _, report := range p.journal.Reports {
		receipt := map[string]string{"reportId": report.ID, "state": report.State}
		if report.Reason != "" {
			receipt["reason"] = report.Reason
		}
		receipts = append(receipts, receipt)
		switch report.State {
		case "observed":
			observed++
		case "undeliverable":
			undeliverable++
		default:
			pending++
		}
	}
	availability := p.lastDigest.Availability
	if availability == "" {
		availability = "unavailable"
	}
	return map[string]any{"id": p.journal.ID, "observed": p.journal.Observed, "execution": p.journal.ObservedExecution, "reportTo": p.journal.ReportTo, "target": p.journal.Target, "state": p.journal.State, "availability": availability, "pending": pending, "observedReports": observed, "undeliverable": undeliverable, "receipts": receipts}
}

func observerPath(dir, id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(dir, "pair-"+hex.EncodeToString(sum[:])+".json")
}

func readObserverFile(path string, limit int64) ([]byte, error) {
	if err := checkMetadataFile(path); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, statErr := file.Stat()
	current, pathErr := os.Lstat(path)
	if err := errors.Join(statErr, pathErr); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if !opened.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(opened, current) || opened.Size() > limit {
		return nil, errors.Join(errors.New("invalid observer metadata file"), file.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return nil, err
	}
	if int64(len(data)) > limit || !utf8.Valid(data) {
		return nil, errors.New("invalid observer metadata")
	}
	return data, nil
}

func observerRootDir(agentDir string, root ChildID, create bool) (string, error) {
	if agentDir == "" || !childUUID.MatchString(string(root)) {
		return "", errors.New("observer storage needs a retained root UUID")
	}
	if create {
		if err := os.MkdirAll(agentDir, 0o700); err != nil {
			return "", err
		}
	}
	if err := checkDir(agentDir, false); err != nil {
		return "", err
	}
	dir := agentDir
	for _, component := range []string{"pig-litter", "observer-roots", string(root)} {
		dir = filepath.Join(dir, component)
		if create {
			err := os.Mkdir(dir, 0o700)
			if err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
			if err == nil {
				if err := syncDir(filepath.Dir(dir)); err != nil {
					return "", err
				}
			}
		}
		if err := checkDir(dir, true); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func canonicalObserverPath(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\r\n\x00") {
		return "", errors.New("invalid observer session path")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

func (o *owner) observerStore(create bool) (*observerStore, error) {
	o.observerMu.Lock()
	defer o.observerMu.Unlock()
	if o.observers != nil {
		return o.observers, nil
	}
	file, err := o.context.SessionManager().GetSessionFile()
	if err != nil || file == nil || *file == "" {
		return nil, reject("unavailable", "observers require a retained root session")
	}
	rootFile, err := canonicalObserverPath(*file)
	if err != nil {
		return nil, err
	}
	dir, err := observerRootDir(o.agentDir, ChildID(o.sessionID), create)
	if err != nil {
		return nil, err
	}
	lease, err := lockTeamFile(filepath.Join(dir, "owner.lock"), create)
	if err != nil {
		return nil, err
	}
	closeOnError := func(err error) (*observerStore, error) { return nil, errors.Join(err, lease.Close()) }
	path := filepath.Join(dir, "identity.json")
	data, err := readObserverFile(path, 4096)
	if errors.Is(err, os.ErrNotExist) && create {
		_, key, keyErr := ed25519.GenerateKey(rand.Reader)
		if keyErr != nil {
			return closeOnError(keyErr)
		}
		identity := observerIdentity{Version: 1, Root: ChildID(o.sessionID), SessionFile: rootFile, Cwd: o.cwd, PrivateKey: key}
		data, err = json.Marshal(identity)
		if err == nil {
			err = publishPeerFile(path, data, false)
		}
	}
	if err != nil {
		return closeOnError(err)
	}
	var identity observerIdentity
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if checkUniqueJSON(data) != nil || decoder.Decode(&identity) != nil || identity.Version != 1 || identity.Root != ChildID(o.sessionID) || identity.SessionFile != rootFile || identity.Cwd != o.cwd || len(identity.PrivateKey) != ed25519.PrivateKeySize || !bytes.Equal(identity.PrivateKey, ed25519.NewKeyFromSeed(identity.PrivateKey[:ed25519.SeedSize])) {
		return closeOnError(errors.New("observer root identity does not match the retained session"))
	}
	ownerID, err := ai.UUIDv7(nil)
	if err != nil {
		return closeOnError(err)
	}
	store := &observerStore{dir: dir, key: ed25519.PrivateKey(identity.PrivateKey), lease: lease, owner: ownerID, pairs: map[string]*observerPair{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return closeOnError(err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "pair-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		store.count++
		pair, err := store.readPair(filepath.Join(dir, entry.Name()), ChildID(o.sessionID))
		if err != nil {
			continue
		}
		pair.store = store
		pair.journal.State = "retired"
		store.pairs[pair.journal.ID] = pair
	}
	o.observers = store
	return store, nil
}

func (s *observerStore) readPair(path string, root ChildID) (*observerPair, error) {
	data, err := readObserverFile(path, observerJournalBytes)
	if err != nil {
		return nil, err
	}
	var signed signedObserverJournal
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if checkUniqueJSON(data) != nil || decoder.Decode(&signed) != nil {
		return nil, errors.New("invalid observer journal")
	}
	sig, err := hex.DecodeString(signed.Signature)
	if err != nil || !ed25519.Verify(s.key.Public().(ed25519.PublicKey), append([]byte("pig-litter-observer-journal-v1\x00"), signed.Journal...), sig) {
		return nil, errors.New("observer journal authentication failed")
	}
	var journal observerJournal
	decoder = json.NewDecoder(bytes.NewReader(signed.Journal))
	decoder.DisallowUnknownFields()
	if checkUniqueJSON(signed.Journal) != nil || decoder.Decode(&journal) != nil || validateObserverJournal(journal, root) != nil || observerPath(s.dir, journal.ID) != path {
		return nil, errors.New("invalid observer journal identity")
	}
	return &observerPair{journal: journal}, nil
}

func validateObserverJournal(j observerJournal, root ChildID) error {
	if j.Version != 1 || j.Root != root || j.ID == "" || len(j.ID) > 128 || j.ObserverExecution != j.ID || j.ObservedExecution == "" || !childUUID.MatchString(string(j.Observer)) || !childUUID.MatchString(string(j.Observed)) || !childUUID.MatchString(string(j.Coordinator)) || !childUUID.MatchString(string(j.Target)) || j.ReportTo != "worker" && j.ReportTo != "coordinator" || j.ReportTo == "worker" && j.Target != j.Observed || j.ReportTo == "coordinator" && j.Target != j.Coordinator || j.State != "pending" && j.State != "armed" && j.State != "retired" || len(j.Reports) > observerReportLimit {
		return errors.New("invalid observer pairing")
	}
	if path, err := canonicalObserverPath(j.TargetFile); err != nil || path != j.TargetFile {
		return errors.New("invalid observer target")
	}
	for _, pin := range []*observerMemberPin{j.ObserverMember, j.ObservedMember, j.CoordinatorMember, j.TargetMember} {
		if pin != nil && (pin.Ref.Team.Root != root || pin.Ref.Team.Epoch != 1 || !childUUID.MatchString(string(pin.Ref.ID)) || pin.Ref.Generation == 0 || pin.Admission == "" || j.TeamCreation == "") {
			return errors.New("invalid observer team identity")
		}
	}
	if j.TeamCreation != "" {
		creation, err := hex.DecodeString(j.TeamCreation)
		if err != nil || len(creation) != 16 || hex.EncodeToString(creation) != j.TeamCreation {
			return errors.New("invalid observer team creation identity")
		}
	}
	if j.TargetMember != nil && j.TargetMember.Ref.ID != j.Target {
		return errors.New("invalid observer team recipient")
	}
	seen := map[string]bool{}
	pending := 0
	for _, report := range j.Reports {
		digest := sha256.Sum256([]byte(report.Text))
		if !observerReportID.MatchString(report.ID) || seen[report.ID] || len(report.Text) > 4096 || strings.TrimSpace(report.Text) == "" || !utf8.ValidString(report.Text) || report.Digest != hex.EncodeToString(digest[:]) {
			return errors.New("invalid observer report")
		}
		seen[report.ID] = true
		switch report.State {
		case "accepted":
			if report.Attempt != nil || report.NativeEntry != "" {
				return errors.New("invalid observer receipt")
			}
			pending++
		case "attempted":
			if report.Attempt == nil || report.Attempt.SessionFile != j.TargetFile || report.NativeEntry != "" || report.Attempt.Owner == "" || report.Attempt.Host.PID <= 0 || report.Attempt.Host.Birth == "" || report.Attempt.Host.Scope == "" {
				return errors.New("invalid observer attempt")
			}
			pending++
		case "observed":
			if report.Attempt == nil || report.NativeEntry == "" {
				return errors.New("invalid observer observation")
			}
		case "undeliverable":
			if report.Reason == "" {
				return errors.New("invalid observer disposition")
			}
		default:
			return errors.New("invalid observer receipt state")
		}
	}
	if pending > observerPendingLimit {
		return errors.New("observer pending limit exceeded")
	}
	return nil
}

func (p *observerPair) saveLocked(create bool) error {
	body, err := json.Marshal(p.journal)
	if err != nil {
		return err
	}
	sig := ed25519.Sign(p.store.key, append([]byte("pig-litter-observer-journal-v1\x00"), body...))
	data, err := json.Marshal(signedObserverJournal{Journal: body, Signature: hex.EncodeToString(sig)})
	if err != nil {
		return err
	}
	if len(data) > observerJournalBytes {
		return errors.New("observer journal exceeds limit")
	}
	return publishPeerFile(observerPath(p.store.dir, p.journal.ID), data, !create)
}

func (o *owner) closeObservers() error {
	o.observerMu.Lock()
	defer o.observerMu.Unlock()
	if o.observers == nil {
		return nil
	}
	err := o.observers.lease.Close()
	o.observers = nil
	return err
}

func (o *owner) prepareObserver(pair *observerPair) error {
	store, err := o.observerStore(true)
	if err != nil {
		return err
	}
	if pair.journal.ObserverMember != nil || pair.journal.ObservedMember != nil || pair.journal.CoordinatorMember != nil {
		team, err := o.teamStoreForRoot()
		if err != nil {
			return err
		}
		creation, valid, err := team.observerMembers(pair.journal.ObservedMember, pair.journal.CoordinatorMember)
		if err != nil || !valid {
			return reject("unavailable", "observed teammate admission is not committed")
		}
		pair.journal.TeamCreation = creation
	}
	var file *string
	if pair.journal.Target == ChildID(o.sessionID) {
		file, err = o.context.SessionManager().GetSessionFile()
	} else {
		o.tree.mu.Lock()
		target := o.tree.records[pair.journal.Target]
		var log *coding.SessionManager
		if target != nil {
			log = target.activeLog
		}
		if log != nil {
			file = log.GetSessionFile()
		}
		o.tree.mu.Unlock()
	}
	if err != nil || file == nil || *file == "" {
		return reject("unavailable", "the selected report target has no retained session yet")
	}
	path, err := canonicalObserverPath(*file)
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.count >= observerPairLimit {
		return reject("capacity", "observer pairing limit reached")
	}
	pair.mu.Lock()
	defer pair.mu.Unlock()
	pair.journal.TargetFile, pair.store = path, store
	store.count++
	if err := pair.saveLocked(true); err != nil {
		return err
	}
	store.pairs[pair.journal.ID] = pair
	return nil
}

func (o *owner) armObserver(pair *observerPair) error {
	o.tree.mu.Lock()
	valid := o.observerPinsLocked(pair, false)
	o.tree.mu.Unlock()
	if !valid {
		return reject("rejected", "the observer pairing changed before admission")
	}
	if err := o.observerTeamAuthority(pair); err != nil {
		return err
	}
	pair.mu.Lock()
	defer pair.mu.Unlock()
	if pair.store == nil || pair.journal.State != "pending" {
		return reject("unavailable", "observer pairing is unavailable")
	}
	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	if o.state.owner != o || o.state.epoch != o.epoch {
		return reject("unavailable", "the root Session retired")
	}
	pair.journal.State = "armed"
	return pair.saveLocked(false)
}

func (o *owner) retireObservers(pairs []*observerPair) error {
	var result error
	seen := map[*observerPair]bool{}
	for _, pair := range pairs {
		if seen[pair] {
			continue
		}
		seen[pair] = true
		pair.mu.Lock()
		if pair.store != nil && pair.journal.State != "retired" {
			pair.journal.State = "retired"
			pair.digestPending = nil
			if pair.digestTimer != nil {
				pair.digestTimer.Stop()
				pair.digestTimer = nil
			}
			result = errors.Join(result, pair.saveLocked(false))
		}
		pair.mu.Unlock()
		o.tree.unindexObserver(pair)
	}
	return result
}

func (t *Tree) indexObserverLocked(pair *observerPair) {
	if t.observerIndex == nil {
		t.observerIndex = map[*run]map[*observerPair]struct{}{}
	}
	for _, subject := range []*run{pair.observer, pair.observed, pair.coordinator, pair.teamAncestor} {
		if subject == nil {
			continue
		}
		if t.observerIndex[subject] == nil {
			t.observerIndex[subject] = map[*observerPair]struct{}{}
		}
		t.observerIndex[subject][pair] = struct{}{}
	}
}

func (t *Tree) unindexObserver(pair *observerPair) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, subject := range []*run{pair.observer, pair.observed, pair.coordinator, pair.teamAncestor} {
		delete(t.observerIndex[subject], pair)
		if len(t.observerIndex[subject]) == 0 {
			delete(t.observerIndex, subject)
		}
	}
}

func (t *Tree) observersFor(subject *run) []*observerPair {
	t.mu.Lock()
	defer t.mu.Unlock()
	pairs := make([]*observerPair, 0, len(t.observerIndex[subject]))
	for pair := range t.observerIndex[subject] {
		pairs = append(pairs, pair)
	}
	return pairs
}

func (t *Tree) retirePausedObservers(runs []*run) {
	if t.observerRetire == nil {
		return
	}
	var pairs []*observerPair
	for _, selected := range runs {
		pairs = append(pairs, t.observersFor(selected)...)
	}
	_ = t.observerRetire(pairs)
}

func (o *owner) observerPinsLocked(pair *observerPair, admitted bool) bool {
	if o.tree.closed || pair == nil || pair.observer == nil || pair.observed == nil {
		return false
	}
	observer, err := o.tree.currentLocked(pair.observer)
	if err != nil || observer.role != "observer" || observer.observer != pair || observer.paused || pair.observer.state != Starting && pair.observer.state != Running || admitted && pair.observer.admission != admissionReady {
		return false
	}
	worker, err := o.tree.currentLocked(pair.observed)
	if err != nil || worker.paused || pair.observed.admission != admissionReady || pair.observed.state != Starting && pair.observed.state != Running || worker.parent != observer.parent {
		return false
	}
	if pair.teamAncestor != nil {
		ancestor, err := o.tree.currentLocked(pair.teamAncestor)
		if err != nil || ancestor.paused || pair.teamAncestor.admission != admissionReady || pair.teamAncestor.state != Starting && pair.teamAncestor.state != Running {
			return false
		}
	}
	if pair.coordinator == nil {
		return observer.parent == o.tree.root && !o.tree.root.paused
	}
	coordinator, err := o.tree.currentLocked(pair.coordinator)
	return err == nil && coordinator == observer.parent && !coordinator.paused && pair.coordinator.admission == admissionReady && (pair.coordinator.state == Starting || pair.coordinator.state == Running)
}

func (o *owner) observerCall(pair *observerPair, run *run) error {
	o.tree.mu.Lock()
	valid := pair != nil && pair.observer == run && o.observerPinsLocked(pair, true)
	o.tree.mu.Unlock()
	if !valid {
		return reject("permission_denied", "observer pairing is retired or unavailable")
	}
	o.state.mu.Lock()
	current := o.state.owner == o && o.state.epoch == o.epoch
	o.state.mu.Unlock()
	if !current {
		return reject("unavailable", "the root Session retired")
	}
	if err := o.observerTeamAuthority(pair); err != nil {
		return err
	}
	pair.mu.Lock()
	armed := pair.journal.State == "armed"
	pair.mu.Unlock()
	if !armed {
		return reject("permission_denied", "observer pairing is retired or unavailable")
	}
	return nil
}

func (o *owner) observerTeamAuthority(pair *observerPair) error {
	if pair.journal.TeamCreation == "" {
		return nil
	}
	o.teamMu.Lock()
	store := o.teamStore
	o.teamMu.Unlock()
	if store == nil {
		return reject("unavailable", "the observer team is unavailable")
	}
	creation, valid, err := store.observerMembers(pair.journal.ObserverMember, pair.journal.ObservedMember, pair.journal.CoordinatorMember)
	if err != nil || !valid || creation != pair.journal.TeamCreation {
		return reject("permission_denied", "the observer team admission is no longer current")
	}
	return nil
}

func parseObserverReport(raw json.RawMessage) (string, string, error) {
	if len(raw) > 6*4096+1024 || !utf8.Valid(raw) || checkUniqueJSON(raw) != nil {
		return "", "", reject("rejected", "invalid observer report arguments")
	}
	fields, err := exactJSONObject(raw, "reportId", "text")
	if err != nil {
		return "", "", reject("rejected", "invalid observer report arguments")
	}
	var id, body string
	if json.Unmarshal(fields["reportId"], &id) != nil || json.Unmarshal(fields["text"], &body) != nil || !observerReportID.MatchString(id) || !utf8.ValidString(body) || len(body) > 4096 || strings.TrimSpace(body) == "" {
		return "", "", reject("rejected", "observer report ID or text is invalid")
	}
	return id, body, nil
}

func (o *owner) acceptObserverReport(ticket *callTicket, raw json.RawMessage) (map[string]any, error) {
	id, body, err := parseObserverReport(raw)
	if err != nil {
		return nil, err
	}
	if ticket == nil || ticket.caller == nil || ticket.scope == nil || ticket.scope.capture(ticket.callID, "observer_report") != ticket {
		return nil, reject("permission_denied", "observer execution is unavailable")
	}
	pair := ticket.caller.owner.observer
	o.tree.lifecycleMu.Lock()
	defer o.tree.lifecycleMu.Unlock()
	if err := o.observerCall(pair, ticket.caller); err != nil {
		return nil, err
	}
	pair.mu.Lock()
	defer pair.mu.Unlock()
	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	if o.state.owner != o || o.state.epoch != o.epoch {
		return nil, reject("unavailable", "the root Session retired")
	}
	pending := 0
	for _, report := range pair.journal.Reports {
		if report.ID == id {
			if report.Text != body {
				return nil, reject("id_conflict", "report ID already has different text")
			}
			if err := pair.saveLocked(false); err != nil {
				return nil, err
			}
			return map[string]any{"reportId": id, "state": "accepted", "delivery": report.State}, nil
		}
		if report.State == "accepted" || report.State == "attempted" {
			pending++
		}
	}
	if len(pair.journal.Reports) >= observerReportLimit || pending >= observerPendingLimit {
		return nil, reject("capacity", "observer report limit reached")
	}
	digest := sha256.Sum256([]byte(body))
	pair.journal.Reports = append(pair.journal.Reports, observerReceipt{ID: id, Text: body, Digest: hex.EncodeToString(digest[:]), State: "accepted"})
	if err := pair.saveLocked(false); err != nil {
		return nil, err
	}
	go o.startObserverRecovery()
	return map[string]any{"reportId": id, "state": "accepted", "delivery": "accepted"}, nil
}

func observerCategory(name string) string {
	switch name {
	case "read", "observer_read":
		return "read"
	case "ls", "find", "grep", "observer_ls":
		return "search"
	case "write", "edit":
		return "write"
	case "bash", "powershell":
		return "shell"
	case "litter_spawn", "team_spawn":
		return "delegation"
	default:
		return "other"
	}
}

func (o *owner) observerActivity(pair *observerPair, run *run) (observerDigest, error) {
	o.tree.lifecycleMu.Lock()
	defer o.tree.lifecycleMu.Unlock()
	if err := o.observerCall(pair, run); err != nil {
		return observerDigest{}, err
	}
	digest := o.captureObserverActivity(pair)
	now := time.Now()
	pair.mu.Lock()
	defer pair.mu.Unlock()
	if pair.lastUpdate.IsZero() || now.Sub(pair.lastUpdate) >= time.Second && !sameObserverActivity(pair.lastDigest, digest) {
		pair.lastDigest = digest
		pair.lastUpdate = now
	}
	return pair.lastDigest, nil
}

func (o *owner) captureObserverActivity(pair *observerPair) observerDigest {
	now := time.Now().UTC()
	o.tree.mu.Lock()
	progress := pair.observed.progress
	state := pair.observed.state
	active := map[string]int{"read": 0, "search": 0, "write": 0, "shell": 0, "delegation": 0, "other": 0}
	for _, name := range progress.activeTools {
		active[observerCategory(name)]++
	}
	o.tree.mu.Unlock()
	sourceAt := progress.sourceAt
	if sourceAt.IsZero() {
		sourceAt = progress.activated
	}
	availability := "available"
	if sourceAt.IsZero() {
		availability = "unavailable"
	} else if now.Sub(sourceAt) > 30*time.Second {
		availability = "stale"
	}
	return observerDigest{Version: progress.version, Observed: pair.journal.Observed, Execution: pair.journal.ObservedExecution, State: state, CapturedAt: now, SourceAt: sourceAt, Availability: availability, Turns: progress.turns, ToolStarts: progress.toolStarts, Active: active, Clipped: progress.toolsClipped}
}

func sameObserverActivity(a, b observerDigest) bool {
	return a.Version == b.Version && a.State == b.State && a.Availability == b.Availability
}

func (o *owner) initialObserverActivity(pair *observerPair) (string, error) {
	o.tree.lifecycleMu.Lock()
	defer o.tree.lifecycleMu.Unlock()
	o.tree.mu.Lock()
	valid := o.observerPinsLocked(pair, true)
	o.tree.mu.Unlock()
	if !valid {
		return "", reject("unavailable", "observer pairing is no longer active")
	}
	digest := o.captureObserverActivity(pair)
	if o.afterObserverCapture != nil {
		o.afterObserverCapture()
	}
	data, err := json.Marshal(digest)
	if err != nil || len(data) > 4096 {
		return "", reject("unavailable", "observer activity is unavailable")
	}
	pair.mu.Lock()
	if pair.journal.State == "retired" {
		pair.mu.Unlock()
		return "", reject("unavailable", "observer pairing is retired")
	}
	pair.lastDigest, pair.lastUpdate = digest, time.Now()
	pair.digestSent, pair.digestSentAt = digest, time.Now()
	if pair.digestPending != nil && pair.digestPending.Version <= digest.Version {
		pair.digestPending = nil
	}
	pair.mu.Unlock()
	o.observerProgress(pair.observed)
	return string(data), nil
}

func (o *owner) observerReady(pair *observerPair) {
	pair.mu.Lock()
	defer pair.mu.Unlock()
	o.scheduleObserverDigestLocked(pair)
}

func (o *owner) observerProgress(selected *run) {
	for _, pair := range o.tree.observersFor(selected) {
		if pair.observed != selected {
			continue
		}
		digest := o.captureObserverActivity(pair)
		pair.mu.Lock()
		if pair.journal.State == "armed" && !pair.digestSentAt.IsZero() && digest.Version >= pair.digestSent.Version && !sameObserverActivity(pair.digestSent, digest) && (pair.digestPending == nil || digest.Version >= pair.digestPending.Version) {
			pair.digestPending = &digest
			o.scheduleObserverDigestLocked(pair)
		}
		pair.mu.Unlock()
	}
}

func (o *owner) scheduleObserverDigestLocked(pair *observerPair) {
	if pair.digestPending == nil || pair.digestInFlight != 0 || pair.digestTimer != nil || pair.journal.State != "armed" {
		return
	}
	delay := time.Until(pair.digestSentAt.Add(time.Second))
	if delay < 0 {
		delay = 0
	}
	pair.digestTimer = time.AfterFunc(delay, func() { o.flushObserverDigest(pair) })
}

func (o *owner) flushObserverDigest(pair *observerPair) {
	pair.mu.Lock()
	pair.digestTimer = nil
	pair.mu.Unlock()
	action, err := o.beginAction(o.tree.ctx)
	if err != nil {
		return
	}
	defer action.finish()
	o.tree.lifecycleMu.Lock()
	defer o.tree.lifecycleMu.Unlock()
	if err := o.observerCall(pair, pair.observer); err != nil {
		return
	}
	pair.observer.mu.Lock()
	session := pair.observer.session
	pair.observer.mu.Unlock()
	if session == nil {
		return
	}
	pair.mu.Lock()
	if pair.digestPending == nil || pair.digestInFlight != 0 || pair.journal.State != "armed" {
		pair.mu.Unlock()
		return
	}
	digest := *pair.digestPending
	pair.digestPending = nil
	pair.digestSequence++
	sequence := pair.digestSequence
	pair.digestInFlight = sequence
	pair.digestSent, pair.digestSentAt = digest, time.Now()
	pair.lastDigest, pair.lastUpdate = digest, pair.digestSentAt
	pair.mu.Unlock()
	content, err := json.Marshal(digest)
	if err == nil && len(content) <= 4096 {
		trigger := false
		err = session.SendCustomMessage(action.ctx, extension.CustomMessageRef{CustomType: "pig_litter_observer_activity", Content: string(content), Details: map[string]any{"pairingId": pair.journal.ID, "sequence": sequence}}, &extension.SendMessageOptions{TriggerTurn: &trigger})
	}
	if err != nil || len(content) > 4096 {
		pair.mu.Lock()
		if pair.digestInFlight == sequence {
			pair.digestInFlight = 0
			if pair.digestPending == nil {
				pair.digestPending = &digest
			}
		}
		pair.mu.Unlock()
	}
}

func (o *owner) observeObserverDigest(run *run, message map[string]any) {
	if run.owner.role != "observer" || message["customType"] != "pig_litter_observer_activity" {
		return
	}
	data, err := json.Marshal(message["details"])
	var details struct {
		PairingID string `json:"pairingId"`
		Sequence  uint64 `json:"sequence"`
	}
	if err != nil || json.Unmarshal(data, &details) != nil {
		return
	}
	pair := run.owner.observer
	pair.mu.Lock()
	defer pair.mu.Unlock()
	if pair.observer == run && pair.journal.ID == details.PairingID && pair.digestInFlight == details.Sequence {
		pair.digestInFlight = 0
		o.scheduleObserverDigestLocked(pair)
	}
}

func (o *owner) observerTools(r *record, run *run, trace *observerCallTrace, available []string) ([]agent.AgentTool, map[string]struct{}) {
	allowed := map[string]struct{}{"observer_report": {}}
	names := []string{"observer_report"}
	for _, item := range []struct{ parent, virtual string }{{"read", "observer_read"}, {"ls", "observer_ls"}} {
		if slices.Contains(available, item.parent) && matchesTool(r.tools, item.parent) && withinParent(r.parent, o.tree.root, item.parent) {
			allowed[item.virtual] = struct{}{}
			names = append(names, item.virtual)
		}
	}
	tools := make([]agent.AgentTool, 0, len(names))
	for _, name := range names {
		tools = append(tools, &nativeTool{name: name, schema: schemas[name], execute: func(_ context.Context, callID string, raw json.RawMessage) (agent.AgentToolResult, error) {
			ticket, err := o.callScope(run).begin(run, callID, name)
			if err != nil {
				return nativeResult(name, nil, err), nil
			}
			defer ticket.scope.forget(ticket)
			if name == "observer_report" {
				original, ok := trace.take(callID)
				originalID, originalText, originalErr := parseObserverReport(original)
				validatedID, validatedText, validatedErr := parseObserverReport(raw)
				if !ok || originalErr != nil || validatedErr != nil || originalID != validatedID || originalText != validatedText {
					return nativeResult(name, nil, reject("rejected", "observer report arguments are invalid or unavailable")), nil
				}
				value, err := o.acceptObserverReport(ticket, raw)
				return nativeResult(name, value, err), nil
			}
			var args struct {
				Path string `json:"path"`
			}
			if !utf8.Valid(raw) || checkUniqueJSON(raw) != nil {
				return nativeResult(name, nil, reject("rejected", "invalid observer path")), nil
			}
			if _, err := exactJSONObject(raw, "path"); err != nil || json.Unmarshal(raw, &args) != nil {
				return nativeResult(name, nil, reject("rejected", "invalid observer path")), nil
			}
			activity, err := o.observerActivity(r.observer, run)
			if err != nil {
				return nativeResult(name, nil, err), nil
			}
			if name == "observer_read" && args.Path == "/observer/activity.json" {
				return nativeResult(name, map[string]any{"activity": activity}, nil), nil
			}
			if name == "observer_ls" && args.Path == "/observer" {
				return nativeResult(name, map[string]any{"entries": []string{"activity.json"}}, nil), nil
			}
			if name == "observer_ls" && args.Path == "/" {
				return nativeResult(name, map[string]any{"entries": []string{"observer/"}}, nil), nil
			}
			return nativeResult(name, nil, reject("permission_denied", "path is outside the observer activity directory")), nil
		}})
	}
	return tools, allowed
}

type observerMarker struct {
	PairingID string  `json:"pairingId"`
	ReportID  string  `json:"reportId"`
	Digest    string  `json:"bodySha256"`
	Recipient ChildID `json:"recipient"`
	Observer  ChildID `json:"observer"`
}

func observerDetails(j observerJournal, report observerReceipt) observerMarker {
	return observerMarker{PairingID: j.ID, ReportID: report.ID, Digest: report.Digest, Recipient: j.Target, Observer: j.Observer}
}

func nativeObserverEntry(path string, expected observerMarker, body, after string) (string, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	info, statErr := file.Stat()
	current, pathErr := os.Lstat(path)
	if err := errors.Join(statErr, pathErr); err != nil {
		return "", errors.Join(err, file.Close())
	}
	if !info.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(info, current) || info.Size() > 32*1024*1024 {
		return "", errors.Join(errors.New("invalid observer recipient transcript"), file.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(file, info.Size()))
	if err := errors.Join(readErr, file.Sync(), file.Close()); err != nil {
		return "", err
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return "", errors.New("observer recipient transcript is incomplete")
	}
	lines := bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'})
	var header struct {
		Type string  `json:"type"`
		ID   ChildID `json:"id"`
	}
	if json.Unmarshal(lines[0], &header) != nil || header.Type != "session" || header.ID != expected.Recipient {
		return "", errors.New("observer recipient session identity differs")
	}
	past := after == ""
	var found string
	for _, line := range lines[1:] {
		var entry struct {
			Type       string          `json:"type"`
			ID         string          `json:"id"`
			CustomType string          `json:"customType"`
			Content    json.RawMessage `json:"content"`
			Details    json.RawMessage `json:"details"`
		}
		if json.Unmarshal(line, &entry) != nil {
			return "", errors.New("invalid observer recipient entry")
		}
		if entry.ID == after {
			past = true
			continue
		}
		if !past || entry.Type != "custom_message" || entry.CustomType != "pig_litter_observer_report" {
			continue
		}
		var marker observerMarker
		var content string
		if json.Unmarshal(entry.Details, &marker) != nil || json.Unmarshal(entry.Content, &content) != nil {
			return "", errors.New("invalid observer report marker")
		}
		if marker.PairingID != expected.PairingID || marker.ReportID != expected.ReportID {
			continue
		}
		if marker != expected || content != body || entry.ID == "" || found != "" {
			return "", errors.New("conflicting observer report marker")
		}
		found = entry.ID
	}
	if !past {
		return "", errors.New("observer delivery boundary is unavailable")
	}
	return found, nil
}

func (o *owner) restoreObservers() error {
	o.observerRestoreMu.Lock()
	defer o.observerRestoreMu.Unlock()
	if o.observersRestored {
		return nil
	}
	dir := filepath.Join(o.agentDir, "pig-litter", "observer-roots", o.sessionID)
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		o.observersRestored = true
		return nil
	} else if err != nil {
		return err
	}
	store, err := o.observerStore(false)
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	o.tree.mu.Lock()
	defer o.tree.mu.Unlock()
	for _, pair := range store.pairs {
		r := o.tree.records[pair.journal.Observer]
		if r != nil && r.observer == nil && r.role == "observer" && r.run.noticeID == pair.journal.ObserverExecution {
			r.observer = pair
		}
	}
	o.observersRestored = true
	return nil
}

func (o *owner) startObserverRecovery() {
	o.state.mu.Lock()
	current := o.state.owner == o && o.state.epoch == o.epoch
	o.state.mu.Unlock()
	if !current {
		return
	}
	o.observerMu.Lock()
	store := o.observers
	o.observerMu.Unlock()
	if store == nil {
		return
	}
	store.mu.Lock()
	if store.recovering {
		store.rerun = true
		store.mu.Unlock()
		return
	}
	store.recovering = true
	store.mu.Unlock()
	action, err := o.beginAction(o.tree.ctx)
	if err != nil {
		store.mu.Lock()
		store.recovering = false
		store.mu.Unlock()
		return
	}
	go func() {
		defer action.finish()
		for {
			store.mu.Lock()
			pairs := make([]*observerPair, 0, len(store.pairs))
			for _, pair := range store.pairs {
				pairs = append(pairs, pair)
			}
			store.rerun = false
			store.mu.Unlock()
			slices.SortFunc(pairs, func(a, b *observerPair) int { return strings.Compare(a.journal.ID, b.journal.ID) })
			type item struct {
				pair *observerPair
				id   string
			}
			var pending []item
			for _, pair := range pairs {
				pair.mu.Lock()
				for _, report := range pair.journal.Reports {
					if report.State == "accepted" || report.State == "attempted" {
						pending = append(pending, item{pair, report.ID})
					}
				}
				pair.mu.Unlock()
			}
			for len(pending) > 0 && action.ctx.Err() == nil {
				batch := min(observerRecoveryBatch, len(pending))
				for _, item := range pending[:batch] {
					o.deliverObserverReport(action.ctx, item.pair, item.id)
				}
				pending = pending[batch:]
			}
			store.mu.Lock()
			if !store.rerun || action.ctx.Err() != nil {
				store.recovering = false
				store.mu.Unlock()
				return
			}
			store.mu.Unlock()
		}
	}()
}

func (o *owner) observerTarget(j observerJournal) (*record, error) {
	o.tree.mu.Lock()
	defer o.tree.mu.Unlock()
	if o.tree.rootID(j.Target) {
		return o.tree.root, nil
	}
	target := o.tree.records[j.Target]
	if target == nil {
		return nil, reject("unavailable", "the frozen report target is unavailable")
	}
	return target, nil
}

func (o *owner) deliverObserverReport(ctx context.Context, pair *observerPair, id string) {
	pair.mu.Lock()
	defer pair.mu.Unlock()
	index := -1
	for i, report := range pair.journal.Reports {
		if report.ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	report := &pair.journal.Reports[index]
	if report.State != "accepted" && report.State != "attempted" {
		return
	}
	reason := func(value string) {
		if report.Reason != value {
			report.Reason = value
			_ = pair.saveLocked(false)
		}
	}
	removed := func() {
		if report.State == "attempted" {
			current, err := embeddedPeerHost()
			prior := report.Attempt
			quiescent := err == nil && !prior.Root && prior.Owner != pair.store.owner && (prior.Host == current || peerHostGone(prior.Host, current))
			if !quiescent {
				reason("previous handoff remains uncertain")
				return
			}
			entry, err := nativeObserverEntry(pair.journal.TargetFile, observerDetails(pair.journal, *report), report.Text, prior.After)
			if err != nil {
				reason("recipient marker could not be reconciled")
				return
			}
			if entry != "" {
				report.State, report.NativeEntry, report.Reason = "observed", entry, ""
				_ = pair.saveLocked(false)
				return
			}
		}
		report.State, report.Reason = "undeliverable", "frozen teammate was removed"
		_ = pair.saveLocked(false)
	}
	if pin := pair.journal.TargetMember; pin != nil {
		o.teamMu.Lock()
		team := o.teamStore
		o.teamMu.Unlock()
		if team == nil {
			path := filepath.Join(o.agentDir, "pig-litter", "roots", o.sessionID)
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				removed()
			} else {
				reason("frozen teammate identity is unavailable")
			}
			return
		}
		creation, valid, err := team.observerMembers(pin)
		if err != nil {
			reason("frozen teammate identity is unavailable")
			return
		}
		if !valid || creation != pair.journal.TeamCreation {
			removed()
			return
		}
	}
	target, err := o.observerTarget(pair.journal)
	if err != nil {
		reason("frozen recipient is unavailable")
		return
	}
	target.deliveryMu.Lock()
	defer target.deliveryMu.Unlock()
	if ctx.Err() != nil || o.current() != nil {
		return
	}
	root := target == o.tree.root
	var host peerHostIdentity
	var session *coding.Session
	var selected *run
	var boundary string
	if root {
		file, err := o.context.SessionManager().GetSessionFile()
		if err != nil || file == nil {
			reason("recipient session file is unavailable")
			return
		}
		path, err := canonicalObserverPath(*file)
		if err != nil || path != pair.journal.TargetFile {
			reason("recipient session identity changed")
			return
		}
		identity, err := o.context.HostProcessIdentity()
		if err != nil {
			reason("recipient writer identity is unavailable")
			return
		}
		host = peerHostIdentity{PID: identity.PID, Birth: identity.Birth, Scope: identity.Scope}
		leaf, err := o.context.SessionManager().GetLeafID()
		if err != nil {
			reason("recipient session boundary is unavailable")
			return
		}
		if leaf != nil {
			boundary = *leaf
		}
	} else {
		host, err = embeddedPeerHost()
		if err != nil {
			reason("recipient writer identity is unavailable")
			return
		}
		o.tree.mu.Lock()
		selected = target.run
		o.tree.mu.Unlock()
		selected.mu.Lock()
		session = selected.session
		selected.mu.Unlock()
		if session != nil {
			file := session.SessionManager().GetSessionFile()
			if file == nil {
				reason("recipient session file is unavailable")
				return
			}
			path, err := canonicalObserverPath(*file)
			if err != nil || path != pair.journal.TargetFile {
				reason("recipient session identity changed")
				return
			}
			if leaf := session.SessionManager().GetLeafID(); leaf != nil {
				boundary = *leaf
			}
		}
	}
	marker := observerDetails(pair.journal, *report)
	if report.State == "attempted" {
		prior := report.Attempt
		quiet := false
		if !prior.Root && !root && prior.Owner == pair.store.owner {
			quiet = session == nil || selected.noticeID != prior.WriterExecution
		} else if !prior.Root && !root && prior.Owner != pair.store.owner && prior.Host == host {
			quiet = true
		} else {
			quiet = peerHostGone(prior.Host, host)
		}
		entry, err := nativeObserverEntry(pair.journal.TargetFile, marker, report.Text, prior.After)
		if err != nil {
			reason("recipient marker could not be reconciled")
			return
		}
		if entry != "" {
			report.State, report.NativeEntry, report.Reason = "observed", entry, ""
			_ = pair.saveLocked(false)
			return
		}
		if !quiet {
			reason("previous handoff remains uncertain")
			return
		}
	}
	if !root && session == nil {
		err := o.withChildLogLocked(target, func(log *coding.SessionManager) error {
			file := log.GetSessionFile()
			if file == nil {
				return errors.New("recipient session file is unavailable")
			}
			path, err := canonicalObserverPath(*file)
			if err != nil || path != pair.journal.TargetFile {
				return errors.New("recipient session identity changed")
			}
			if leaf := log.GetLeafID(); leaf != nil {
				boundary = *leaf
			}
			report.State = "attempted"
			report.Attempt = &observerAttempt{Host: host, SessionFile: path, After: boundary, Owner: pair.store.owner, WriterExecution: selected.noticeID}
			report.Reason = ""
			if err := pair.saveLocked(false); err != nil {
				return err
			}
			_, err = log.AppendCustomMessageEntry("pig_litter_observer_report", report.Text, true, marker)
			return err
		})
		if err != nil {
			reason("recipient handoff remains unresolved")
			return
		}
	} else {
		writerExecution := "root"
		if selected != nil {
			writerExecution = selected.noticeID
		}
		report.State = "attempted"
		report.Attempt = &observerAttempt{Host: host, SessionFile: pair.journal.TargetFile, After: boundary, Owner: pair.store.owner, WriterExecution: writerExecution, Root: root}
		report.Reason = ""
		if err := pair.saveLocked(false); err != nil {
			return
		}
		trigger := false
		if root {
			options := sdk.SendMessageOptions{TriggerTurn: &trigger}
			if o.context.Mode() == "tui" {
				options.DeliverAs = "nextTurn"
			}
			err = o.context.SendCustomMessage(sdk.CustomMessage{CustomType: "pig_litter_observer_report", Content: report.Text, Display: true, Details: marker}, options)
		} else {
			err = session.SendCustomMessage(ctx, extension.CustomMessageRef{CustomType: "pig_litter_observer_report", Content: report.Text, Display: true, Details: marker}, &extension.SendMessageOptions{TriggerTurn: &trigger})
		}
		if err != nil {
			reason("recipient handoff remains unresolved")
			return
		}
	}
	entry, err := nativeObserverEntry(pair.journal.TargetFile, marker, report.Text, report.Attempt.After)
	if err != nil {
		reason("recipient marker could not be reconciled")
		return
	}
	if entry != "" {
		report.State, report.NativeEntry, report.Reason = "observed", entry, ""
		_ = pair.saveLocked(false)
	} else {
		reason("previous handoff remains uncertain")
	}
}
