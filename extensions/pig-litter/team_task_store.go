package piglitter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

const teamTaskJSONLimit = 8 * 1024 * 1024

func (s *teamStore) taskTransaction(change func(*teamTaskGraph) (bool, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("team store is closed")
	}
	if err := checkLockedFile(s.lead); err != nil {
		return fmt.Errorf("team owner lease: %w", err)
	}
	rosterLock, err := lockTeamFile(filepath.Join(s.dir, "team.lock"), false)
	if err != nil {
		return err
	}
	roster, readErr := loadTeamRoster(s.dir, s.root)
	if err := errors.Join(readErr, rosterLock.Close()); err != nil {
		return err
	}
	if roster.OwnerEpoch != s.ownerEpoch || roster.OwnerToken != s.ownerToken {
		return errors.New("team owner lease is stale")
	}
	lockPath := filepath.Join(s.dir, "tasks.lock")
	_, statErr := os.Lstat(lockPath)
	initial := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !initial {
		return statErr
	}
	if initial {
		if _, err := os.Lstat(filepath.Join(s.dir, "tasks.json")); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return errors.New("task lock is missing for existing task metadata")
			}
			return err
		}
	}
	lock, err := lockTeamFile(lockPath, true)
	if err != nil {
		return err
	}
	err = func() error {
		if initial {
			graph := teamTaskGraph{Version: 1, Team: roster.Team, Tasks: []teamTask{}}
			if err := saveTeamTasks(s.dir, graph); err != nil {
				return err
			}
		}
		graph, err := loadTeamTasks(s.dir, s.root)
		if err != nil {
			return err
		}
		changed, err := change(&graph)
		if err != nil || !changed {
			return err
		}
		if err := validateTeamTasks(graph, s.root); err != nil {
			return err
		}
		return saveTeamTasks(s.dir, graph)
	}()
	return errors.Join(err, lock.Close())
}

func (s *teamStore) createTask(subject, description string, dependencies []TaskID, metadata map[string]string) (teamTaskView, error) {
	var result teamTaskView
	err := s.taskTransaction(func(graph *teamTaskGraph) (bool, error) {
		if strings.TrimSpace(subject) == "" || len(subject) > 256 || !utf8.ValidString(subject) || len(description) > 16384 || !utf8.ValidString(description) {
			return false, reject("rejected", "task subject or description is invalid")
		}
		if len(dependencies) > 128 || len(metadata) > 64 {
			return false, reject("rejected", "too many task dependencies or metadata entries")
		}
		seen := make(map[TaskID]bool, len(dependencies))
		for _, dependency := range dependencies {
			if seen[dependency] || graph.find(dependency) < 0 {
				return false, reject("rejected", "task dependency is missing, duplicated, or belongs to another team")
			}
			seen[dependency] = true
		}
		for key, value := range metadata {
			if strings.TrimSpace(key) == "" || len(key) > 128 || len(value) > 4096 || !utf8.ValidString(key) || !utf8.ValidString(value) {
				return false, reject("rejected", "task metadata is invalid")
			}
		}
		if graph.HighWater == math.MaxUint64 {
			return false, errors.New("task ID allocation exhausted")
		}
		graph.HighWater++
		if dependencies == nil {
			dependencies = []TaskID{}
		}
		if metadata == nil {
			metadata = map[string]string{}
		}
		graph.Tasks = append(graph.Tasks, teamTask{ID: TaskID{Team: graph.Team, Number: graph.HighWater}, Subject: subject, Description: description, State: TaskPending, BlockedBy: dependencies, Metadata: metadata})
		result = graph.view(len(graph.Tasks) - 1)
		return true, nil
	})
	return result, err
}

func (s *teamStore) listTasks() ([]teamTaskView, error) {
	var result []teamTaskView
	err := s.taskTransaction(func(graph *teamTaskGraph) (bool, error) {
		result = make([]teamTaskView, 0, len(graph.Tasks))
		for i := range graph.Tasks {
			result = append(result, graph.view(i))
		}
		return false, nil
	})
	return result, err
}

func (s *teamStore) claimTask(id TaskID, member MemberRef, explicit bool) (teamTaskView, string, error) {
	var result teamTaskView
	var state string
	err := s.taskTransaction(func(graph *teamTaskGraph) (bool, error) {
		index := graph.find(id)
		if index < 0 || member.Team != graph.Team {
			return false, reject("rejected", "task or teammate is unavailable in this team")
		}
		result = graph.view(index)
		if result.Owner != nil {
			state = "already_claimed"
			return false, nil
		}
		if result.State != TaskPending {
			return false, reject("rejected", "task is not pending")
		}
		if result.Blocked && !explicit {
			state = "blocked"
			return false, nil
		}
		graph.Tasks[index].Owner = &member
		graph.Tasks[index].State = TaskInProgress
		result = graph.view(index)
		state = "claimed"
		if explicit {
			state = "assigned"
		}
		return true, nil
	})
	return result, state, err
}

func (s *teamStore) completeTask(id TaskID, actor *MemberRef) (teamTaskView, string, error) {
	var result teamTaskView
	var state string
	err := s.taskTransaction(func(graph *teamTaskGraph) (bool, error) {
		index := graph.find(id)
		if index < 0 {
			return false, reject("rejected", "task is unavailable in this team")
		}
		result = graph.view(index)
		if actor != nil && (result.Owner == nil || *result.Owner != *actor) {
			return false, reject("permission_denied", "only the task owner or team lead can complete it")
		}
		if result.State == TaskCompleted {
			state = "already_completed"
			return false, nil
		}
		if result.Blocked {
			state = "blocked"
			return false, nil
		}
		graph.Tasks[index].State = TaskCompleted
		result = graph.view(index)
		state = "completed"
		return true, nil
	})
	return result, state, err
}

func (s *teamStore) deleteTask(id TaskID) (teamTaskView, error) {
	var result teamTaskView
	err := s.taskTransaction(func(graph *teamTaskGraph) (bool, error) {
		index := graph.find(id)
		if index < 0 {
			return false, reject("rejected", "task is unavailable in this team")
		}
		for _, task := range graph.Tasks {
			if slices.Contains(task.BlockedBy, id) {
				return false, reject("rejected", "another task depends on this task")
			}
		}
		result = graph.view(index)
		graph.Tasks = slices.Delete(graph.Tasks, index, index+1)
		return true, nil
	})
	return result, err
}

func loadTeamTasks(dir string, root ChildID) (teamTaskGraph, error) {
	path := filepath.Join(dir, "tasks.json")
	if err := checkMetadataFile(path); err != nil {
		return teamTaskGraph{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return teamTaskGraph{}, err
	}
	opened, statErr := file.Stat()
	current, pathErr := os.Lstat(path)
	if err := errors.Join(statErr, pathErr); err != nil {
		return teamTaskGraph{}, errors.Join(err, file.Close())
	}
	if !opened.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return teamTaskGraph{}, errors.Join(errors.New("task metadata file was replaced"), file.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(file, teamTaskJSONLimit+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return teamTaskGraph{}, err
	}
	if len(data) > teamTaskJSONLimit {
		return teamTaskGraph{}, errors.New("task metadata exceeds size limit")
	}
	if err := checkUniqueJSON(data); err != nil {
		return teamTaskGraph{}, fmt.Errorf("invalid task metadata: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var graph teamTaskGraph
	if err := decoder.Decode(&graph); err != nil {
		return teamTaskGraph{}, fmt.Errorf("invalid task metadata: %w", err)
	}
	if err := validateTeamTasks(graph, root); err != nil {
		return teamTaskGraph{}, err
	}
	return graph, nil
}

func validateTeamTasks(graph teamTaskGraph, root ChildID) error {
	if graph.Version != 1 || graph.Team != (TeamID{Root: root, Epoch: 1}) || graph.Tasks == nil {
		return errors.New("invalid task metadata header")
	}
	seen := make(map[TaskID]bool, len(graph.Tasks))
	for _, task := range graph.Tasks {
		if task.ID.Team != graph.Team || task.ID.Number == 0 || task.ID.Number > graph.HighWater || seen[task.ID] || strings.TrimSpace(task.Subject) == "" || len(task.Subject) > 256 || !utf8.ValidString(task.Subject) || len(task.Description) > 16384 || !utf8.ValidString(task.Description) || task.BlockedBy == nil || task.Metadata == nil || len(task.BlockedBy) > 128 || len(task.Metadata) > 64 {
			return errors.New("invalid task record")
		}
		if task.State != TaskPending && task.State != TaskInProgress && task.State != TaskCompleted {
			return errors.New("invalid task state")
		}
		if task.Owner != nil && (task.Owner.Team != graph.Team || !childUUID.MatchString(string(task.Owner.ID)) || task.Owner.Generation == 0) {
			return errors.New("invalid task owner")
		}
		for key, value := range task.Metadata {
			if strings.TrimSpace(key) == "" || len(key) > 128 || len(value) > 4096 || !utf8.ValidString(key) || !utf8.ValidString(value) {
				return errors.New("invalid task metadata")
			}
		}
		seen[task.ID] = true
	}
	for _, task := range graph.Tasks {
		dependencies := map[TaskID]bool{}
		for _, dependency := range task.BlockedBy {
			if !seen[dependency] || dependencies[dependency] || dependency.Number >= task.ID.Number {
				return errors.New("invalid task dependency")
			}
			dependencies[dependency] = true
		}
	}
	return nil
}

func saveTeamTasks(dir string, graph teamTaskGraph) error {
	data, err := json.Marshal(graph)
	if err != nil {
		return err
	}
	if len(data) > teamTaskJSONLimit {
		return errors.New("task metadata exceeds size limit")
	}
	file, err := os.CreateTemp(dir, ".tasks-*.tmp")
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
	if err := os.Rename(name, filepath.Join(dir, "tasks.json")); err != nil {
		return err
	}
	return syncDir(dir)
}
