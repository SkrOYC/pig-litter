package piglitter

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/gofrs/flock"
)

const (
	teamStoreVersion = 1
	teamMemberLimit  = 16
	teamJSONLimit    = 128 * 1024
)

type teamRoster struct {
	Version      uint64            `json:"version"`
	Team         TeamID            `json:"team"`
	Revision     uint64            `json:"revision"`
	OwnerEpoch   uint64            `json:"ownerEpoch"`
	OwnerToken   string            `json:"ownerToken"`
	OwnerPID     int               `json:"ownerPid"`
	Generation   uint64            `json:"generationHighWater"`
	Members      []teamMember      `json:"members"`
	Reservations []teamReservation `json:"reservations"`
}

type teamReservation struct {
	Member        teamMember `json:"member"`
	RequestedName string     `json:"requestedName"`
}

type teamStore struct {
	mu         sync.Mutex
	dir        string
	root       ChildID
	ownerEpoch uint64
	ownerToken string
	lead       *flock.Flock
	closed     bool
}

func openTeamStore(agentDir string, root ChildID) (*teamStore, error) {
	if !childUUID.MatchString(string(root)) {
		return nil, errors.New("invalid team root UUID")
	}
	dir, created, err := createTeamDir(agentDir, root)
	if err != nil {
		return nil, err
	}
	lead, err := lockTeamFile(filepath.Join(dir, "lead.lock"), true)
	if err != nil {
		return nil, fmt.Errorf("acquire team owner lease: %w", err)
	}
	store := &teamStore{dir: dir, root: root, lead: lead}
	if err := store.claim(created); err != nil {
		return nil, errors.Join(err, lead.Close())
	}
	return store, nil
}

func readTeamSnapshot(agentDir string, root ChildID) (TeamID, []teamMember, error) {
	if !childUUID.MatchString(string(root)) {
		return TeamID{}, nil, errors.New("invalid team root UUID")
	}
	dir := filepath.Join(agentDir, "pig-litter", "roots", string(root))
	if err := checkTeamDirs(agentDir, dir); err != nil {
		return TeamID{}, nil, err
	}
	if err := checkMetadataFile(filepath.Join(dir, "lead.lock")); err != nil {
		return TeamID{}, nil, err
	}
	lock, err := lockTeamFile(filepath.Join(dir, "team.lock"), false)
	if err != nil {
		return TeamID{}, nil, err
	}
	roster, readErr := loadTeamRoster(dir, root)
	if err := errors.Join(readErr, lock.Close()); err != nil {
		return TeamID{}, nil, err
	}
	return roster.Team, slices.Clone(roster.Members), nil
}

func (s *teamStore) claim(created bool) error {
	lock, err := lockTeamFile(filepath.Join(s.dir, "team.lock"), true)
	if err != nil {
		return err
	}
	err = s.claimLocked(created)
	if closeErr := lock.Close(); closeErr != nil {
		return errors.Join(err, closeErr)
	}
	return err
}

func (s *teamStore) claimLocked(created bool) error {
	var roster teamRoster
	var err error
	if created {
		if _, err := os.Lstat(filepath.Join(s.dir, "team.json")); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return errors.New("new team directory already contains metadata")
			}
			return err
		}
		roster = teamRoster{Version: teamStoreVersion, Team: TeamID{Root: s.root, Epoch: 1}, Members: []teamMember{}, Reservations: []teamReservation{}}
	} else {
		roster, err = loadTeamRoster(s.dir, s.root)
		if err != nil {
			return err
		}
	}
	if roster.OwnerEpoch == math.MaxUint64 || roster.Revision == math.MaxUint64 {
		return errors.New("team owner epoch or revision exhausted")
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	roster.OwnerEpoch++
	roster.Revision++
	roster.OwnerToken = hex.EncodeToString(token)
	roster.OwnerPID = os.Getpid()
	roster.Reservations = []teamReservation{}
	for i := range roster.Members {
		if roster.Members[i].State == "running" || roster.Members[i].State == "starting" {
			roster.Members[i].State = "interrupted"
		}
	}
	if err := saveTeamRoster(s.dir, roster); err != nil {
		return err
	}
	s.ownerEpoch, s.ownerToken = roster.OwnerEpoch, roster.OwnerToken
	return nil
}

func (s *teamStore) Reserve(id ChildID, admissionID, name string, workspace WorkspacePolicy) (teamMember, error) {
	var member teamMember
	err := s.transact(func(roster *teamRoster) (bool, error) {
		if err := validateTeamName(name); err != nil {
			return false, err
		}
		for _, existing := range roster.Reservations {
			if existing.Member.AdmissionID == admissionID && existing.Member.Ref.ID == id && existing.RequestedName == name && existing.Member.Workspace == workspace {
				member = existing.Member
				return false, nil
			}
		}
		if len(roster.Members)+len(roster.Reservations) >= teamMemberLimit {
			return false, reject("capacity", "team member limit reached")
		}
		if roster.Generation == math.MaxUint64 {
			return false, errors.New("team member generation exhausted")
		}
		allocated, err := allocateStoreName(roster, name)
		if err != nil {
			return false, err
		}
		roster.Generation++
		member = teamMember{Ref: MemberRef{Team: roster.Team, ID: id, Generation: roster.Generation}, AdmissionID: admissionID, Name: allocated, Workspace: workspace, State: "starting"}
		roster.Reservations = append(roster.Reservations, teamReservation{Member: member, RequestedName: name})
		return true, nil
	})
	return member, err
}

func allocateStoreName(roster *teamRoster, requested string) (string, error) {
	names := make(map[string]bool, len(roster.Members)+len(roster.Reservations))
	for _, member := range roster.Members {
		names[strings.ToLower(member.Name)] = true
	}
	for _, pending := range roster.Reservations {
		names[strings.ToLower(pending.Member.Name)] = true
	}
	if !names[strings.ToLower(requested)] {
		return requested, nil
	}
	for suffix := 2; ; suffix++ {
		candidate := requested + "-" + strconv.Itoa(suffix)
		if err := validateTeamName(candidate); err != nil {
			return "", reject("name_exhausted", "team member name cannot be extended")
		}
		if !names[strings.ToLower(candidate)] {
			return candidate, nil
		}
	}
}

func (s *teamStore) Commit(admissionID string) (teamMember, error) {
	var member teamMember
	err := s.transact(func(roster *teamRoster) (bool, error) {
		for _, existing := range roster.Members {
			if existing.AdmissionID == admissionID {
				member = existing
				return false, nil
			}
		}
		for i, pending := range roster.Reservations {
			if pending.Member.AdmissionID == admissionID {
				pending.Member.State = "running"
				member = pending.Member
				roster.Reservations = slices.Delete(roster.Reservations, i, i+1)
				roster.Members = append(roster.Members, pending.Member)
				return true, nil
			}
		}
		return false, errors.New("team admission not found")
	})
	return member, err
}

func (s *teamStore) Rollback(admissionID string) error {
	return s.transact(func(roster *teamRoster) (bool, error) {
		for _, member := range roster.Members {
			if member.AdmissionID == admissionID {
				return false, errors.New("cannot roll back a committed team member")
			}
		}
		for i, pending := range roster.Reservations {
			if pending.Member.AdmissionID == admissionID {
				roster.Reservations = slices.Delete(roster.Reservations, i, i+1)
				return true, nil
			}
		}
		return false, nil
	})
}

func (s *teamStore) SetState(ref MemberRef, state string) error {
	if state != "idle" && state != "interrupted" && state != "unavailable" && state != "paused" {
		return errors.New("invalid team member state transition")
	}
	return s.transact(func(roster *teamRoster) (bool, error) {
		for i := range roster.Members {
			if roster.Members[i].Ref == ref {
				if roster.Members[i].State == state {
					return false, nil
				}
				roster.Members[i].State = state
				return true, nil
			}
		}
		return false, errors.New("team member reference not found")
	})
}

func (s *teamStore) Snapshot() (TeamID, []teamMember, error) {
	var team TeamID
	var members []teamMember
	err := s.transact(func(roster *teamRoster) (bool, error) {
		team, members = roster.Team, slices.Clone(roster.Members)
		return false, nil
	})
	return team, members, err
}

func (s *teamStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if err := s.lead.Close(); err != nil {
		return err
	}
	s.closed = true
	return nil
}

func (s *teamStore) transact(change func(*teamRoster) (bool, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("team store is closed")
	}
	if err := checkLockedFile(s.lead); err != nil {
		return fmt.Errorf("team owner lease: %w", err)
	}
	lock, err := lockTeamFile(filepath.Join(s.dir, "team.lock"), false)
	if err != nil {
		return err
	}
	err = func() error {
		roster, err := loadTeamRoster(s.dir, s.root)
		if err != nil {
			return err
		}
		if roster.OwnerEpoch != s.ownerEpoch || roster.OwnerToken != s.ownerToken {
			return errors.New("team owner lease is stale")
		}
		changed, err := change(&roster)
		if err != nil || !changed {
			return err
		}
		if roster.Revision == math.MaxUint64 {
			return errors.New("team revision exhausted")
		}
		roster.Revision++
		if err := validateTeamRoster(roster, s.root); err != nil {
			return err
		}
		return saveTeamRoster(s.dir, roster)
	}()
	if closeErr := lock.Close(); closeErr != nil {
		return errors.Join(err, closeErr)
	}
	return err
}

func createTeamDir(agentDir string, root ChildID) (string, bool, error) {
	if agentDir == "" {
		return "", false, errors.New("empty agent directory")
	}
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return "", false, err
	}
	if err := checkDir(agentDir, false); err != nil {
		return "", false, err
	}
	dir := agentDir
	created := false
	for _, name := range []string{"pig-litter", "roots", string(root)} {
		dir = filepath.Join(dir, name)
		err := os.Mkdir(dir, 0o700)
		if err != nil && !errors.Is(err, os.ErrExist) {
			return "", false, err
		}
		created = err == nil
		if err := checkDir(dir, true); err != nil {
			return "", false, err
		}
		if created {
			if err := syncDir(filepath.Dir(dir)); err != nil {
				return "", false, err
			}
		}
	}
	return dir, created, nil
}

func checkTeamDirs(agentDir, dir string) error {
	if agentDir == "" {
		return errors.New("empty agent directory")
	}
	for _, item := range []struct {
		path    string
		private bool
	}{{agentDir, false}, {filepath.Join(agentDir, "pig-litter"), true}, {filepath.Join(agentDir, "pig-litter", "roots"), true}, {dir, true}} {
		if err := checkDir(item.path, item.private); err != nil {
			return err
		}
	}
	return nil
}

func checkDir(path string, private bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || private && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("invalid team directory %q", path)
	}
	return nil
}

func lockTeamFile(path string, create bool) (*flock.Flock, error) {
	if create {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if err := errors.Join(file.Sync(), file.Close(), syncDir(filepath.Dir(path))); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	if err := checkMetadataFile(path); err != nil {
		return nil, err
	}
	lock := flock.New(path, flock.SetFlag(os.O_RDWR), flock.SetPermissions(0o600))
	ok, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("team lock %q is held", path)
	}
	if err := checkLockedFile(lock); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	return lock, nil
}

func checkMetadataFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("invalid team metadata file %q", path)
	}
	return nil
}

func checkLockedFile(lock *flock.Flock) error {
	if !lock.Locked() {
		return errors.New("team lock is not held")
	}
	if err := checkMetadataFile(lock.Path()); err != nil {
		return err
	}
	pathInfo, err := os.Lstat(lock.Path())
	if err != nil {
		return err
	}
	lockedInfo, err := lock.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(pathInfo, lockedInfo) {
		return errors.New("team lock file was replaced")
	}
	return nil
}

func loadTeamRoster(dir string, root ChildID) (teamRoster, error) {
	path := filepath.Join(dir, "team.json")
	if err := checkMetadataFile(path); err != nil {
		return teamRoster{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return teamRoster{}, err
	}
	opened, statErr := file.Stat()
	current, pathErr := os.Lstat(path)
	if err := errors.Join(statErr, pathErr); err != nil {
		return teamRoster{}, errors.Join(err, file.Close())
	}
	if !opened.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return teamRoster{}, errors.Join(errors.New("team metadata file was replaced"), file.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(file, teamJSONLimit+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return teamRoster{}, err
	}
	if len(data) > teamJSONLimit {
		return teamRoster{}, errors.New("team metadata exceeds size limit")
	}
	if err := checkUniqueJSON(data); err != nil {
		return teamRoster{}, fmt.Errorf("invalid team metadata: %w", err)
	}
	if err := checkRosterJSONSchema(data); err != nil {
		return teamRoster{}, fmt.Errorf("invalid team metadata: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var roster teamRoster
	if err := decoder.Decode(&roster); err != nil {
		return teamRoster{}, fmt.Errorf("invalid team metadata: %w", err)
	}
	if err := validateTeamRoster(roster, root); err != nil {
		return teamRoster{}, err
	}
	return roster, nil
}

func validateTeamRoster(roster teamRoster, root ChildID) error {
	if roster.Version != teamStoreVersion || roster.Team != (TeamID{Root: root, Epoch: 1}) || roster.Revision == 0 || roster.OwnerEpoch == 0 || roster.OwnerPID <= 0 || len(roster.OwnerToken) != 32 || roster.Members == nil || roster.Reservations == nil || len(roster.Members)+len(roster.Reservations) > teamMemberLimit {
		return errors.New("invalid team metadata header")
	}
	if _, err := hex.DecodeString(roster.OwnerToken); err != nil || strings.ToLower(roster.OwnerToken) != roster.OwnerToken {
		return errors.New("invalid team owner token")
	}
	names := make(map[string]bool)
	identities := map[string]bool{string(root): true}
	generations := make(map[uint64]bool)
	reservations := make([]teamMember, 0, len(roster.Reservations))
	for _, reservation := range roster.Reservations {
		if err := validateTeamName(reservation.RequestedName); err != nil || !allocatedStoreName(reservation.RequestedName, reservation.Member.Name) {
			return errors.New("invalid team reservation name")
		}
		reservations = append(reservations, reservation.Member)
	}
	for _, group := range []struct {
		members []teamMember
		pending bool
	}{{roster.Members, false}, {reservations, true}} {
		for _, member := range group.members {
			if member.Ref.Team != roster.Team || !childUUID.MatchString(string(member.Ref.ID)) || !childUUID.MatchString(member.AdmissionID) || member.Ref.Generation == 0 || member.Ref.Generation > roster.Generation || generations[member.Ref.Generation] || identities[string(member.Ref.ID)] || identities[member.AdmissionID] || string(member.Ref.ID) == member.AdmissionID {
				return errors.New("invalid or duplicate team identity")
			}
			if err := validateTeamName(member.Name); err != nil || names[strings.ToLower(member.Name)] {
				return errors.New("invalid or duplicate team name")
			}
			if member.Workspace.Kind != "shared_checkout" || !filepath.IsAbs(member.Workspace.Cwd) || filepath.Clean(member.Workspace.Cwd) != member.Workspace.Cwd || len(member.Workspace.Cwd) > 4096 || !utf8.ValidString(member.Workspace.Cwd) || strings.ContainsRune(member.Workspace.Cwd, 0) {
				return errors.New("invalid team workspace")
			}
			if group.pending && member.State != "starting" || !group.pending && !validStoredMemberState(member.State) {
				return errors.New("invalid team member state")
			}
			names[strings.ToLower(member.Name)] = true
			identities[string(member.Ref.ID)], identities[member.AdmissionID] = true, true
			generations[member.Ref.Generation] = true
		}
	}
	return nil
}

func allocatedStoreName(requested, actual string) bool {
	if requested == actual {
		return true
	}
	suffix, ok := strings.CutPrefix(actual, requested+"-")
	if !ok || strings.HasPrefix(suffix, "0") {
		return false
	}
	number, err := strconv.ParseUint(suffix, 10, 64)
	return err == nil && number >= 2
}

func validStoredMemberState(state string) bool {
	switch state {
	case "starting", "running", "idle", "interrupted", "unavailable", "paused":
		return true
	default:
		return false
	}
}

func saveTeamRoster(dir string, roster teamRoster) error {
	data, err := json.Marshal(roster)
	if err != nil {
		return err
	}
	if len(data) > teamJSONLimit {
		return errors.New("team metadata exceeds size limit")
	}
	file, err := os.CreateTemp(dir, ".team-*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(dir, "team.json")); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func checkUniqueJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := readUniqueJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func checkRosterJSONSchema(data []byte) error {
	root, err := exactJSONObject(data, "version", "team", "revision", "ownerEpoch", "ownerToken", "ownerPid", "generationHighWater", "members", "reservations")
	if err != nil {
		return err
	}
	if _, err := exactJSONObject(root["team"], "root", "epoch"); err != nil {
		return err
	}
	for _, key := range []string{"members", "reservations"} {
		var members []json.RawMessage
		if err := json.Unmarshal(root[key], &members); err != nil || members == nil {
			return errors.New("invalid team member array")
		}
		for _, data := range members {
			if key == "reservations" {
				reservation, err := exactJSONObject(data, "member", "requestedName")
				if err != nil {
					return err
				}
				data = reservation["member"]
			}
			member, err := exactJSONObject(data, "ref", "admissionId", "name", "workspace", "state")
			if err != nil {
				return err
			}
			ref, err := exactJSONObject(member["ref"], "team", "id", "generation")
			if err != nil {
				return err
			}
			if _, err := exactJSONObject(ref["team"], "root", "epoch"); err != nil {
				return err
			}
			if _, err := exactJSONObject(member["workspace"], "kind", "cwd"); err != nil {
				return err
			}
		}
	}
	return nil
}

func exactJSONObject(data []byte, keys ...string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || len(object) != len(keys) {
		return nil, errors.New("invalid team metadata object")
	}
	for _, key := range keys {
		if _, ok := object[key]; !ok {
			return nil, errors.New("missing or unexpected team metadata field")
		}
	}
	return object, nil
}

func readUniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := make(map[string]bool)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || keys[name] {
				return errors.New("duplicate or invalid JSON object key")
			}
			keys[name] = true
			if err := readUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := readUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON value")
	}
	_, err = decoder.Token()
	return err
}
