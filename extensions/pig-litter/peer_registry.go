//go:build linux || darwin

package piglitter

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

const (
	localRegistryLimit = 512
	localPageLimit     = 50
	localSnapshotLimit = 8
	localRecordLimit   = 16 * 1024
	localPresenceTTL   = 10 * time.Second
	localSnapshotTTL   = 5 * time.Second
)

type localSessionRef struct {
	Session ChildID `json:"session"`
	Key     string  `json:"key"`
}

func (r localSessionRef) String() string {
	return "local:" + string(r.Session) + "@" + r.Key
}

func parseLocalSessionRef(value string) (localSessionRef, error) {
	var ref localSessionRef
	if !strings.HasPrefix(value, "local:") {
		return ref, errors.New("invalid local session reference")
	}
	session, key, ok := strings.Cut(strings.TrimPrefix(value, "local:"), "@")
	ref = localSessionRef{Session: ChildID(session), Key: key}
	if !ok || !validLocalRef(ref) {
		return localSessionRef{}, errors.New("invalid local session reference")
	}
	return ref, nil
}

func validLocalRef(ref localSessionRef) bool {
	key, err := hex.DecodeString(ref.Key)
	return childUUID.MatchString(string(ref.Session)) && err == nil && len(key) == ed25519.PublicKeySize && hex.EncodeToString(key) == ref.Key
}

type localPeerOptions struct {
	RegistryDir       string
	StateDir          string
	Session           ChildID
	SessionFile       string
	CWD               string
	Name              string
	AcceptInbound     bool
	NativeSessionHost *localProcessIdentity
}

type localProcessIdentity struct {
	PID   int    `json:"pid"`
	Birth string `json:"birth"`
	Scope string `json:"scope"`
}

func validLocalProcessIdentity(identity localProcessIdentity) bool {
	return identity.PID > 0 && identity.Birth != "" && len(identity.Birth) <= 128 && identity.Scope != "" && len(identity.Scope) <= 256
}

type localIdentity struct {
	Version     int     `json:"version"`
	Session     ChildID `json:"session"`
	SessionFile string  `json:"sessionFile"`
	CWD         string  `json:"cwd"`
	PrivateKey  []byte  `json:"privateKey"`
}

type localPresence struct {
	Version       int                   `json:"version"`
	Ref           localSessionRef       `json:"ref"`
	Name          string                `json:"name"`
	Incarnation   string                `json:"incarnation"`
	Socket        string                `json:"socket"`
	PID           int                   `json:"pid"`
	NativeHost    *localProcessIdentity `json:"nativeHost,omitempty"`
	AcceptInbound bool                  `json:"acceptInbound"`
	Expires       time.Time             `json:"expires"`
	Signature     string                `json:"signature"`
}

type localDiscoveryRow struct {
	Ref           localSessionRef `json:"ref"`
	Name          string          `json:"name"`
	AcceptInbound bool            `json:"acceptInbound"`
}

type localDiscoveryIssue struct {
	Record string `json:"record"`
	Reason string `json:"reason"`
}

type localDiscoveryPage struct {
	Rows      []localDiscoveryRow   `json:"rows"`
	Issues    []localDiscoveryIssue `json:"issues"`
	Truncated bool                  `json:"truncated"`
	Next      string                `json:"next,omitempty"`
}

type localSnapshot struct {
	rows      []localDiscoveryRow
	issues    []localDiscoveryIssue
	truncated bool
	expires   time.Time
}

type localPeer struct {
	mu            sync.Mutex
	registryDir   string
	stateDir      string
	sessionFile   string
	name          string
	ref           localSessionRef
	privateKey    ed25519.PrivateKey
	incarnation   string
	accept        bool
	nativeHost    *localProcessIdentity
	activity      localActivity
	lease         *flock.Flock
	registryLease *flock.Flock
	transport     *localTransport
	outgoing      sync.WaitGroup
	watches       sync.WaitGroup
	watching      map[string]bool
	closed        bool
	snapshots     map[string]localSnapshot
	lastSenders   map[string]time.Time
	stop          chan struct{}
	stopped       chan struct{}
	noticeWake    chan struct{}
	noticeStopped chan struct{}
}

func startLocalPeer(options localPeerOptions) (*localPeer, error) {
	if !childUUID.MatchString(string(options.Session)) || options.SessionFile == "" || options.CWD == "" {
		return nil, errors.New("local peers require a retained native session")
	}
	if err := validateTeamName(options.Name); err != nil {
		return nil, err
	}
	cwd, err := canonicalTeamDirectory(options.CWD)
	if err != nil {
		return nil, err
	}
	file, err := canonicalLocalFile(options.SessionFile)
	if err != nil {
		return nil, err
	}
	registry, err := prepareLocalDirectory(options.RegistryDir)
	if err != nil {
		return nil, err
	}
	if _, err := prepareLocalDirectory(filepath.Join(registry, "records")); err != nil {
		return nil, err
	}
	if _, err := prepareLocalDirectory(filepath.Join(registry, "locks")); err != nil {
		return nil, err
	}
	state, err := prepareLocalDirectory(options.StateDir)
	if err != nil {
		return nil, err
	}
	state, err = prepareLocalDirectory(filepath.Join(state, string(options.Session)))
	if err != nil {
		return nil, err
	}
	lease, err := lockTeamFile(filepath.Join(state, "owner.lock"), true)
	if err != nil {
		return nil, fmt.Errorf("acquire local session owner: %w", err)
	}
	registryLease, err := lockTeamFile(filepath.Join(registry, "locks", string(options.Session)+".lock"), true)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("acquire local registry owner: %w", err), lease.Close())
	}
	peer := &localPeer{
		registryDir: registry, stateDir: state, sessionFile: file, name: options.Name,
		accept: options.AcceptInbound, lease: lease, registryLease: registryLease, snapshots: make(map[string]localSnapshot),
		lastSenders: make(map[string]time.Time), stop: make(chan struct{}), stopped: make(chan struct{}),
		watching: make(map[string]bool), noticeWake: make(chan struct{}, 1), noticeStopped: make(chan struct{}),
	}
	if options.NativeSessionHost != nil {
		if !validLocalProcessIdentity(*options.NativeSessionHost) {
			return nil, errors.Join(errors.New("invalid native host identity"), registryLease.Close(), lease.Close())
		}
		host := *options.NativeSessionHost
		peer.nativeHost = &host
	}
	if err := peer.loadIdentity(options.Session, file, cwd); err != nil {
		return nil, errors.Join(err, registryLease.Close(), lease.Close())
	}
	peer.incarnation, err = localRandom(16)
	if err != nil {
		return nil, errors.Join(err, registryLease.Close(), lease.Close())
	}
	peer.transport, err = startLocalTransport(peer)
	if err != nil {
		return nil, errors.Join(err, registryLease.Close(), lease.Close())
	}
	if err := peer.writePresence(); err != nil {
		return nil, errors.Join(err, peer.transport.close(), registryLease.Close(), lease.Close())
	}
	go peer.heartbeat()
	go peer.driveLocalNotices()
	peer.wakeLocalNotices()
	peer.watches.Add(1)
	go func() {
		defer peer.watches.Done()
		peer.recoverLocalWatches()
	}()
	return peer, nil
}

func prepareLocalDirectory(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("local peer directory must be absolute")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 || !localOwned(info) {
		return "", fmt.Errorf("local peer directory is not private: %s", path)
	}
	return filepath.Clean(path), nil
}

func canonicalLocalFile(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Base(path) == "." || strings.ContainsAny(path, "\r\n") {
		return "", errors.New("invalid native session path")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

func localRandom(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func (p *localPeer) loadIdentity(session ChildID, file, cwd string) error {
	path := filepath.Join(p.stateDir, "identity.json")
	data, err := readLocalFile(path, localRecordLimit)
	if errors.Is(err, os.ErrNotExist) {
		_, private, keyErr := ed25519.GenerateKey(rand.Reader)
		if keyErr != nil {
			return keyErr
		}
		identity := localIdentity{Version: 1, Session: session, SessionFile: file, CWD: cwd, PrivateKey: private}
		data, err = json.Marshal(identity)
		if err == nil {
			err = publishPeerFile(path, data, false)
		}
	}
	if err != nil {
		return err
	}
	var identity localIdentity
	if err := decodeLocalJSON(data, &identity); err != nil {
		return err
	}
	if identity.Version != 1 || identity.Session != session || identity.SessionFile != file || identity.CWD != cwd || len(identity.PrivateKey) != ed25519.PrivateKeySize {
		return errors.New("retained local identity does not match the native session")
	}
	private := ed25519.PrivateKey(identity.PrivateKey)
	derived := ed25519.NewKeyFromSeed(private.Seed())
	if !bytes.Equal(private, derived) {
		return errors.New("invalid retained local identity key")
	}
	if err := syncDir(p.stateDir); err != nil {
		return err
	}
	p.privateKey = private
	p.ref = localSessionRef{Session: session, Key: hex.EncodeToString(private.Public().(ed25519.PublicKey))}
	return nil
}

func readLocalFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !localOwned(info) || info.Size() > limit {
		return nil, errors.New("invalid local peer metadata")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.Join(errors.New("local peer metadata was replaced"), err, file.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("local peer metadata exceeds limit")
	}
	return data, nil
}

func decodeLocalJSON(data []byte, out any) error {
	if err := checkUniqueJSON(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(out)
}

func (p *localPeer) presencePath() string {
	return filepath.Join(p.registryDir, "records", string(p.ref.Session)+".json")
}

func (p *localPeer) socketName() string {
	return "s-" + p.incarnation
}

func localPresenceBytes(presence localPresence) ([]byte, error) {
	presence.Signature = ""
	return json.Marshal(presence)
}

func (p *localPeer) writePresence() error {
	presence := localPresence{Version: 1, Ref: p.ref, Name: p.name, Incarnation: p.incarnation, Socket: p.socketName(), PID: os.Getpid(), NativeHost: p.nativeHost, AcceptInbound: p.accept, Expires: time.Now().Add(localPresenceTTL).UTC()}
	data, err := localPresenceBytes(presence)
	if err != nil {
		return err
	}
	presence.Signature = hex.EncodeToString(ed25519.Sign(p.privateKey, append([]byte("pig-litter-presence-v1\x00"), data...)))
	data, err = json.Marshal(presence)
	if err != nil {
		return err
	}
	return publishPeerFile(p.presencePath(), data, true)
}

func readLocalPresence(path string, now time.Time) (localPresence, error) {
	var presence localPresence
	data, err := readLocalFile(path, localRecordLimit)
	if err != nil {
		return presence, err
	}
	if err := decodeLocalJSON(data, &presence); err != nil {
		return presence, err
	}
	if presence.Version != 1 || !validLocalRef(presence.Ref) || validateTeamName(presence.Name) != nil || presence.PID <= 0 || len(presence.Incarnation) != 32 || presence.Socket != "s-"+presence.Incarnation || filepath.Base(path) != string(presence.Ref.Session)+".json" {
		return presence, errors.New("invalid local presence")
	}
	if _, err := hex.DecodeString(presence.Incarnation); err != nil || !presence.Expires.After(now) || presence.Expires.After(now.Add(2*localPresenceTTL)) {
		return presence, errors.New("expired or invalid local presence")
	}
	if presence.NativeHost != nil && !validLocalProcessIdentity(*presence.NativeHost) {
		return presence, errors.New("invalid native host identity")
	}
	public, _ := hex.DecodeString(presence.Ref.Key)
	sig, err := hex.DecodeString(presence.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return presence, errors.New("invalid local presence signature")
	}
	data, err = localPresenceBytes(presence)
	if err != nil || !ed25519.Verify(public, append([]byte("pig-litter-presence-v1\x00"), data...), sig) {
		return presence, errors.New("unverified local presence")
	}
	return presence, nil
}

func (p *localPeer) heartbeat() {
	defer close(p.stopped)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.mu.Lock()
			if !p.closed {
				_ = p.writePresence()
			}
			p.mu.Unlock()
		case <-p.stop:
			return
		}
	}
}

func (p *localPeer) discover(cursor string) (localDiscoveryPage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return localDiscoveryPage{}, errors.New("local peer is closed")
	}
	now := time.Now()
	for key, snapshot := range p.snapshots {
		if !snapshot.expires.After(now) {
			delete(p.snapshots, key)
		}
	}
	if cursor == "" {
		if len(p.snapshots) >= localSnapshotLimit {
			return localDiscoveryPage{}, errors.New("local discovery snapshot capacity reached")
		}
		snapshot, err := p.scanPresence(now)
		if err != nil {
			return localDiscoveryPage{}, err
		}
		if len(snapshot.rows) <= localPageLimit {
			return localDiscoveryPage{Rows: snapshot.rows, Issues: snapshot.issues, Truncated: snapshot.truncated}, nil
		}
		key, err := localRandom(16)
		if err != nil {
			return localDiscoveryPage{}, err
		}
		p.snapshots[key] = snapshot
		return localDiscoveryPage{Rows: slices.Clone(snapshot.rows[:localPageLimit]), Issues: snapshot.issues, Truncated: snapshot.truncated, Next: key + ":" + strconv.Itoa(localPageLimit)}, nil
	}
	key, offsetText, ok := strings.Cut(cursor, ":")
	offset, err := strconv.Atoi(offsetText)
	snapshot, found := p.snapshots[key]
	if !ok || err != nil || !found || offset < localPageLimit || offset >= len(snapshot.rows) || offset%localPageLimit != 0 {
		return localDiscoveryPage{}, errors.New("local discovery cursor expired or invalid")
	}
	end := min(offset+localPageLimit, len(snapshot.rows))
	page := localDiscoveryPage{Rows: slices.Clone(snapshot.rows[offset:end]), Issues: snapshot.issues, Truncated: snapshot.truncated}
	if end < len(snapshot.rows) {
		page.Next = key + ":" + strconv.Itoa(end)
	} else {
		delete(p.snapshots, key)
	}
	return page, nil
}

func (p *localPeer) scanPresence(now time.Time) (localSnapshot, error) {
	result := localSnapshot{expires: now.Add(localSnapshotTTL), rows: []localDiscoveryRow{}, issues: []localDiscoveryIssue{}}
	dir, err := os.Open(filepath.Join(p.registryDir, "records"))
	if err != nil {
		return result, err
	}
	entries, readErr := dir.ReadDir(localRegistryLimit + 1)
	if err := errors.Join(readErr, dir.Close()); err != nil && !errors.Is(err, io.EOF) {
		return result, err
	}
	if len(entries) > localRegistryLimit {
		result.truncated = true
		entries = entries[:localRegistryLimit]
	}
	for _, entry := range entries {
		name := entry.Name()
		presence, err := readLocalPresence(filepath.Join(p.registryDir, "records", name), now)
		if err != nil {
			result.issues = append(result.issues, localDiscoveryIssue{Record: name, Reason: "unreadable, invalid, or stale"})
			continue
		}
		if err := p.checkLocalSocket(presence); err != nil {
			result.issues = append(result.issues, localDiscoveryIssue{Record: name, Reason: "socket unavailable"})
			continue
		}
		if presence.Ref != p.ref {
			result.rows = append(result.rows, localDiscoveryRow{Ref: presence.Ref, Name: presence.Name, AcceptInbound: presence.AcceptInbound})
		}
	}
	slices.SortFunc(result.rows, func(a, b localDiscoveryRow) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.Ref.String(), b.Ref.String())
	})
	return result, nil
}

func (p *localPeer) lookup(ref localSessionRef) (localPresence, error) {
	if !validLocalRef(ref) || ref == p.ref {
		return localPresence{}, errors.New("invalid local destination")
	}
	presence, err := readLocalPresence(filepath.Join(p.registryDir, "records", string(ref.Session)+".json"), time.Now())
	if err != nil {
		return localPresence{}, err
	}
	if presence.Ref != ref {
		return localPresence{}, errLocalStale
	}
	if err := p.checkLocalSocket(presence); err != nil {
		return localPresence{}, err
	}
	return presence, nil
}

func (p *localPeer) checkLocalSocket(presence localPresence) error {
	info, err := os.Lstat(filepath.Join(p.registryDir, presence.Socket))
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 || !localOwned(info) {
		return errors.New("invalid local peer socket")
	}
	return nil
}

func (p *localPeer) close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	close(p.stop)
	p.mu.Unlock()
	<-p.stopped
	transportErr := p.transport.close()
	<-p.noticeStopped
	p.outgoing.Wait()
	p.watches.Wait()
	presence, err := readLocalPresence(p.presencePath(), time.Now())
	if err == nil && presence.Ref == p.ref && presence.Incarnation == p.incarnation {
		err = os.Remove(p.presencePath())
		if err == nil {
			err = syncDir(filepath.Dir(p.presencePath()))
		}
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return errors.Join(transportErr, err, p.registryLease.Close(), p.lease.Close())
}

func localDigest(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}
