package piglitter

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func storeTestUUID(n int) ChildID {
	return ChildID(fmt.Sprintf("00000000-0000-7000-8000-%012x", n))
}

func storeTestOpen(t *testing.T) (*teamStore, string, ChildID, WorkspacePolicy) {
	t.Helper()
	dir := t.TempDir()
	root := storeTestUUID(1)
	store, err := openTeamStore(dir, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, dir, root, WorkspacePolicy{Kind: "shared_checkout", Cwd: t.TempDir()}
}

func TestTeamStorePublishesOnlyCommittedMembers(t *testing.T) {
	store, dir, root, workspace := storeTestOpen(t)
	id, admission := storeTestUUID(2), string(storeTestUUID(3))
	pending, err := store.Reserve(id, admission, "parser", workspace)
	if err != nil {
		t.Fatal(err)
	}
	wantRef := MemberRef{Team: TeamID{Root: root, Epoch: 1}, ID: id, Generation: 1}
	if pending.Ref != wantRef || pending.State != "starting" || pending.AdmissionID != admission {
		t.Fatalf("reservation = %#v", pending)
	}
	team, members, err := readTeamSnapshot(dir, root)
	if err != nil || team != wantRef.Team || len(members) != 0 {
		t.Fatalf("passive snapshot before commit = %#v %#v, %v", team, members, err)
	}
	if retry, err := store.Reserve(id, admission, "parser", workspace); err != nil || retry != pending {
		t.Fatalf("reservation retry = %#v, %v", retry, err)
	}
	if _, err := store.Commit(string(storeTestUUID(4))); err == nil {
		t.Fatal("committed a different admission")
	}
	committed, err := store.Commit(admission)
	if err != nil || committed.Ref != wantRef || committed.State != "running" {
		t.Fatalf("commit = %#v, %v", committed, err)
	}
	if retry, err := store.Commit(admission); err != nil || retry != committed {
		t.Fatalf("commit retry = %#v, %v", retry, err)
	}
	if err := store.Rollback(admission); err == nil {
		t.Fatal("rolled back a committed member")
	}
	if err := store.SetState(MemberRef{Team: wantRef.Team, ID: id, Generation: 2}, "idle"); err == nil {
		t.Fatal("accepted stale generation")
	}
	if err := store.SetState(wantRef, "idle"); err != nil {
		t.Fatal(err)
	}
	team, members, err = readTeamSnapshot(dir, root)
	if err != nil || team != wantRef.Team || len(members) != 1 || members[0].Ref != wantRef || members[0].State != "idle" || members[0].Workspace != workspace {
		t.Fatalf("passive snapshot after settlement = %#v %#v, %v", team, members, err)
	}
	members[0].Name = "changed"
	_, again, err := store.Snapshot()
	if err != nil || len(again) != 1 || again[0].Name != "parser" {
		t.Fatalf("snapshot mutation changed roster = %#v, %v", again, err)
	}
	rootDir := filepath.Join(dir, "pig-litter", "roots", string(root))
	for _, file := range []string{"team.json", "team.lock", "lead.lock"} {
		info, err := os.Lstat(filepath.Join(rootDir, file))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("private metadata %s = %v, %v", file, info, err)
		}
	}
	info, err := os.Stat(rootDir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("private team directory = %v, %v", info, err)
	}
}

func TestTeamStoreBoundsConcurrentReservationsAndNeverReusesGenerations(t *testing.T) {
	store, dir, root, workspace := storeTestOpen(t)
	type result struct {
		member teamMember
		err    error
	}
	results := make(chan result, 32)
	var workers sync.WaitGroup
	for i := range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			member, err := store.Reserve(storeTestUUID(100+i), string(storeTestUUID(200+i)), fmt.Sprintf("worker-%d", i), workspace)
			results <- result{member, err}
		}()
	}
	workers.Wait()
	close(results)
	var accepted []teamMember
	var rejected int
	for result := range results {
		if result.err != nil {
			rejected++
		} else {
			accepted = append(accepted, result.member)
		}
	}
	if len(accepted) != 16 || rejected != 16 {
		t.Fatalf("accepted %d, rejected %d", len(accepted), rejected)
	}
	_, members, err := readTeamSnapshot(dir, root)
	if err != nil || len(members) != 0 {
		t.Fatalf("reservations leaked into passive snapshot: %#v, %v", members, err)
	}
	generations := make([]uint64, 0, len(accepted))
	for _, member := range accepted {
		generations = append(generations, member.Ref.Generation)
	}
	slices.Sort(generations)
	for i, generation := range generations {
		if generation != uint64(i+1) {
			t.Fatalf("allocated generations = %v", generations)
		}
	}
	first := accepted[0]
	if _, err := store.Commit(first.AdmissionID); err != nil {
		t.Fatal(err)
	}
	for _, member := range accepted[1:] {
		if err := store.Rollback(member.AdmissionID); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Rollback(accepted[1].AdmissionID); err != nil {
		t.Fatalf("repeated rollback = %v", err)
	}
	next, err := store.Reserve(storeTestUUID(500), string(storeTestUUID(501)), "replacement", workspace)
	if err != nil || next.Ref.Generation != 17 {
		t.Fatalf("generation after rollback = %#v, %v", next, err)
	}
	_, members, err = store.Snapshot()
	if err != nil || len(members) != 1 || members[0].Ref != first.Ref {
		t.Fatalf("rollback changed committed member = %#v, %v", members, err)
	}
}

func TestTeamStoreRejectsDuplicateIdentities(t *testing.T) {
	store, _, root, workspace := storeTestOpen(t)
	id, admission := storeTestUUID(2), string(storeTestUUID(3))
	if _, err := store.Reserve(id, admission, "Parser", workspace); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		id        ChildID
		admission string
		label     string
	}{
		{"member", id, string(storeTestUUID(5)), "other"},
		{"admission", storeTestUUID(4), admission, "other"},
		{"root", root, string(storeTestUUID(5)), "other"},
		{"cross identity", ChildID(admission), string(storeTestUUID(5)), "other"},
		{"bad UUID", "short", string(storeTestUUID(5)), "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.Reserve(tc.id, tc.admission, tc.label, workspace); err == nil {
				t.Fatalf("accepted duplicate or invalid %s", tc.name)
			}
		})
	}
	if err := store.Rollback(admission); err != nil {
		t.Fatal(err)
	}
	next, err := store.Reserve(storeTestUUID(6), string(storeTestUUID(7)), "parser", workspace)
	if err != nil || next.Ref.Generation != 2 {
		t.Fatalf("failed reservations changed generation or retained name: %#v, %v", next, err)
	}
}

func TestTeamStoreAllocatesNamesAcrossCommittedAndProvisionalMembers(t *testing.T) {
	store, _, _, workspace := storeTestOpen(t)
	first, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "parser", workspace)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Reserve(storeTestUUID(4), string(storeTestUUID(5)), "PARSER", workspace)
	if err != nil || second.Name != "PARSER-2" {
		t.Fatalf("provisional name collision = %#v, %v", second, err)
	}
	if retry, err := store.Reserve(storeTestUUID(4), string(storeTestUUID(5)), "PARSER", workspace); err != nil || retry != second {
		t.Fatalf("suffixed reservation retry = %#v, %v", retry, err)
	}
	if _, err := store.Commit(first.AdmissionID); err != nil {
		t.Fatal(err)
	}
	third, err := store.Reserve(storeTestUUID(6), string(storeTestUUID(7)), "Parser", workspace)
	if err != nil || third.Name != "Parser-3" {
		t.Fatalf("committed and provisional name collision = %#v, %v", third, err)
	}
	longName := strings.Repeat("x", 64)
	if _, err := store.Reserve(storeTestUUID(8), string(storeTestUUID(9)), longName, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reserve(storeTestUUID(10), string(storeTestUUID(11)), longName, workspace); err == nil {
		t.Fatal("accepted a suffix past the name limit")
	} else if failure, ok := err.(*treeError); !ok || failure.code != "name_exhausted" {
		t.Fatalf("overflow error = %v", err)
	}
}

func TestTeamStoreRecoveryPreservesMembersAndDiscardsReservations(t *testing.T) {
	store, dir, root, workspace := storeTestOpen(t)
	running, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "running", workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(running.AdmissionID); err != nil {
		t.Fatal(err)
	}
	idle, err := store.Reserve(storeTestUUID(4), string(storeTestUUID(5)), "idle", workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(idle.AdmissionID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetState(idle.Ref, "idle"); err != nil {
		t.Fatal(err)
	}
	pending, err := store.Reserve(storeTestUUID(6), string(storeTestUUID(7)), "pending", workspace)
	if err != nil {
		t.Fatal(err)
	}
	oldEpoch := store.ownerEpoch
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := openTeamStore(dir, root)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	team, members, err := recovered.Snapshot()
	if err != nil || team != running.Ref.Team || recovered.ownerEpoch != oldEpoch+1 || len(members) != 2 {
		t.Fatalf("recovered team = %#v %#v epoch %d, %v", team, members, recovered.ownerEpoch, err)
	}
	if members[0].Ref != running.Ref || members[0].State != "interrupted" || members[1].Ref != idle.Ref || members[1].State != "idle" {
		t.Fatalf("recovered members = %#v", members)
	}
	if _, err := recovered.Commit(pending.AdmissionID); err == nil {
		t.Fatal("recovered provisional admission")
	}
	next, err := recovered.Reserve(storeTestUUID(8), string(storeTestUUID(9)), "pending", workspace)
	if err != nil || next.Ref.Generation != 4 {
		t.Fatalf("generation after recovery = %#v, %v", next, err)
	}
	if err := store.SetState(running.Ref, "idle"); err == nil {
		t.Fatal("closed owner changed recovered member")
	}
}

func TestTeamStoreLeaseProcess(t *testing.T) {
	if os.Getenv("PIG_TEAM_STORE_HELPER") == "1" {
		store, err := openTeamStore(os.Getenv("PIG_TEAM_STORE_DIR"), ChildID(os.Getenv("PIG_TEAM_STORE_ROOT")))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stdout, "ready")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		_ = store.Close()
		os.Exit(0)
	}
	store, dir, root, workspace := storeTestOpen(t)
	active, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "active", workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(active.AdmissionID); err != nil {
		t.Fatal(err)
	}
	rootDir := filepath.Join(dir, "pig-litter", "roots", string(root))
	leadBefore, err := os.Stat(filepath.Join(rootDir, "lead.lock"))
	if err != nil {
		t.Fatal(err)
	}
	teamBefore, err := os.Stat(filepath.Join(rootDir, "team.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openTeamStore(dir, root); err == nil {
		t.Fatal("second owner acquired a live lease")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestTeamStoreLeaseProcess$")
	cmd.Env = append(os.Environ(), "PIG_TEAM_STORE_HELPER=1", "PIG_TEAM_STORE_DIR="+dir, "PIG_TEAM_STORE_ROOT="+string(root))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = stdin.Close() })
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- line
	}()
	select {
	case line := <-ready:
		if line != "ready\n" {
			t.Fatalf("child did not claim lease: %q", line)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("child did not claim lease")
	}
	if _, err := openTeamStore(dir, root); err == nil {
		t.Fatal("another process acquired the live lease")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	recovered, err := openTeamStore(dir, root)
	if err != nil {
		t.Fatalf("owner could not recover after process death: %v", err)
	}
	defer recovered.Close()
	_, members, err := recovered.Snapshot()
	if err != nil || len(members) != 1 || members[0].Ref != active.Ref || members[0].State != "interrupted" {
		t.Fatalf("recovery after process death = %#v, %v", members, err)
	}
	leadAfter, err := os.Stat(filepath.Join(rootDir, "lead.lock"))
	if err != nil || !os.SameFile(leadBefore, leadAfter) {
		t.Fatalf("lead lock inode changed: %v", err)
	}
	teamAfter, err := os.Stat(filepath.Join(rootDir, "team.lock"))
	if err != nil || !os.SameFile(teamBefore, teamAfter) {
		t.Fatalf("team lock inode changed: %v", err)
	}
}

func TestTeamStoreRefusesCorruptMetadataWithoutOverwriting(t *testing.T) {
	cases := []struct {
		name   string
		change func(map[string]any)
	}{
		{"version", func(v map[string]any) { v["version"] = 2 }},
		{"root", func(v map[string]any) { v["team"].(map[string]any)["root"] = string(storeTestUUID(999)) }},
		{"missing", func(v map[string]any) { delete(v, "generationHighWater") }},
		{"unknown", func(v map[string]any) { v["unknown"] = true }},
		{"case variant", func(v map[string]any) { v["OwnerEpoch"] = v["ownerEpoch"]; delete(v, "ownerEpoch") }},
		{"high water", func(v map[string]any) { v["generationHighWater"] = 0 }},
		{"member state", func(v map[string]any) { v["members"].([]any)[0].(map[string]any)["state"] = "mystery" }},
		{"workspace", func(v map[string]any) {
			v["members"].([]any)[0].(map[string]any)["workspace"].(map[string]any)["cwd"] = "relative"
		}},
		{"duplicate member", func(v map[string]any) { members := v["members"].([]any); v["members"] = append(members, members[0]) }},
		{"null reservations", func(v map[string]any) { v["reservations"] = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, dir, root, workspace := storeTestOpen(t)
			member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "parser", workspace)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Commit(member.AdmissionID); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "pig-litter", "roots", string(root), "team.json")
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var data map[string]any
			if err := json.Unmarshal(original, &data); err != nil {
				t.Fatal(err)
			}
			tc.change(data)
			corrupt, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, corrupt, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := readTeamSnapshot(dir, root); err == nil {
				t.Fatal("passive snapshot accepted corrupted metadata")
			}
			if _, err := openTeamStore(dir, root); err == nil {
				t.Fatal("owner accepted corrupted metadata")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(corrupt) {
				t.Fatalf("corrupted source was overwritten: %q, %v", after, err)
			}
		})
	}
	for _, payload := range []string{"{", "null", `{"version":1,"version":1}`, `{} {}`} {
		t.Run("syntax "+payload, func(t *testing.T) {
			store, dir, root, _ := storeTestOpen(t)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "pig-litter", "roots", string(root), "team.json")
			if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := openTeamStore(dir, root); err == nil {
				t.Fatalf("accepted %q", payload)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != payload {
				t.Fatalf("invalid source overwritten: %q, %v", after, err)
			}
		})
	}
}

func TestTeamStoreRejectsMalformedFilesAndLockContention(t *testing.T) {
	t.Run("metadata symlink", func(t *testing.T) {
		store, dir, root, _ := storeTestOpen(t)
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "pig-litter", "roots", string(root), "team.json")
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := openTeamStore(dir, root); err == nil {
			t.Fatal("accepted a metadata symlink")
		}
		if data, err := os.ReadFile(target); err != nil || string(data) != "untouched" {
			t.Fatalf("symlink target changed = %q, %v", data, err)
		}
	})
	t.Run("root directory symlink", func(t *testing.T) {
		dir := t.TempDir()
		root := storeTestUUID(1)
		roots := filepath.Join(dir, "pig-litter", "roots")
		if err := os.MkdirAll(roots, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), filepath.Join(roots, string(root))); err != nil {
			t.Fatal(err)
		}
		if _, err := openTeamStore(dir, root); err == nil {
			t.Fatal("accepted a symlinked team directory")
		}
	})
	t.Run("transaction lock", func(t *testing.T) {
		store, dir, root, workspace := storeTestOpen(t)
		path := filepath.Join(dir, "pig-litter", "roots", string(root), "team.lock")
		lock := flock.New(path)
		ok, err := lock.TryLock()
		if err != nil || !ok {
			t.Fatalf("take transaction lock = %v, %v", ok, err)
		}
		if _, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "parser", workspace); err == nil {
			t.Fatal("wrote through held transaction lock")
		}
		if _, _, err := readTeamSnapshot(dir, root); err == nil {
			t.Fatal("read through held transaction lock")
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "parser", workspace); err != nil {
			t.Fatalf("did not recover after release: %v", err)
		}
	})
	t.Run("missing metadata", func(t *testing.T) {
		store, dir, root, _ := storeTestOpen(t)
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "pig-litter", "roots", string(root), "team.json")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if _, err := openTeamStore(dir, root); err == nil {
			t.Fatal("reinitialized a team with missing metadata")
		}
	})
	t.Run("replaced owner lock", func(t *testing.T) {
		store, dir, root, workspace := storeTestOpen(t)
		path := filepath.Join(dir, "pig-litter", "roots", string(root), "lead.lock")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "parser", workspace); err == nil || !strings.Contains(err.Error(), "replaced") {
			t.Fatalf("accepted a replaced owner lock: %v", err)
		}
	})
}
