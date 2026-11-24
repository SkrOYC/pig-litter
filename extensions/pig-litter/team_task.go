package piglitter

type TaskID struct {
	Team   TeamID `json:"team"`
	Number uint64 `json:"number"`
}

type TaskState string

const (
	TaskPending    TaskState = "pending"
	TaskInProgress TaskState = "in_progress"
	TaskCompleted  TaskState = "completed"
)

type teamTask struct {
	ID          TaskID            `json:"id"`
	Subject     string            `json:"subject"`
	Description string            `json:"description"`
	State       TaskState         `json:"state"`
	Owner       *MemberRef        `json:"owner"`
	BlockedBy   []TaskID          `json:"blockedBy"`
	Metadata    map[string]string `json:"metadata"`
}

type teamTaskView struct {
	teamTask
	Blocked bool `json:"blocked"`
}

type teamTaskEdit struct {
	Subject      *string            `json:"subject"`
	Description  *string            `json:"description"`
	AddBlockedBy []TaskID           `json:"addBlockedBy"`
	AddBlocks    []TaskID           `json:"addBlocks"`
	Metadata     map[string]*string `json:"metadata"`
}

type teamTaskCreation struct {
	ID           TaskID    `json:"id"`
	Caller       MemberRef `json:"caller"`
	SubmissionID string    `json:"submissionId"`
	Digest       string    `json:"digest"`
}

type teamTaskGraph struct {
	Version   uint64                      `json:"version"`
	Revision  uint64                      `json:"revision,omitempty"`
	Team      TeamID                      `json:"team"`
	HighWater uint64                      `json:"highWater"`
	Tasks     []teamTask                  `json:"tasks"`
	Creations map[string]teamTaskCreation `json:"creations,omitempty"`
}

func (g *teamTaskGraph) states() map[TaskID]TaskState {
	states := make(map[TaskID]TaskState, len(g.Tasks))
	for _, existing := range g.Tasks {
		states[existing.ID] = existing.State
	}
	return states
}

func taskView(task teamTask, states map[TaskID]TaskState) teamTaskView {
	blocked := false
	for _, dependency := range task.BlockedBy {
		if states[dependency] != TaskCompleted {
			blocked = true
			break
		}
	}
	return teamTaskView{teamTask: task, Blocked: blocked}
}

func (g *teamTaskGraph) view(index int) teamTaskView {
	return taskView(g.Tasks[index], g.states())
}

func (g *teamTaskGraph) views() []teamTaskView {
	states := g.states()
	views := make([]teamTaskView, 0, len(g.Tasks))
	for _, task := range g.Tasks {
		views = append(views, taskView(task, states))
	}
	return views
}

func (g *teamTaskGraph) find(id TaskID) int {
	for i := range g.Tasks {
		if g.Tasks[i].ID == id {
			return i
		}
	}
	return -1
}

func (o *owner) applyTaskCompletion(authority controlAuthority, id TaskID) (teamTaskView, string, error) {
	if !childUUID.MatchString(string(id.Team.Root)) || id.Team.Epoch == 0 || id.Number == 0 {
		return teamTaskView{}, "", reject("rejected", "invalid task identity")
	}
	o.tree.lifecycleMu.Lock()
	defer o.tree.lifecycleMu.Unlock()
	if err := o.current(); err != nil {
		return teamTaskView{}, "", err
	}
	o.tree.mu.Lock()
	var actor *MemberRef
	if authority.human != nil {
		if err := authority.human(); err != nil {
			o.tree.mu.Unlock()
			return teamTaskView{}, "", err
		}
	} else {
		parent, err := o.tree.callerLocked(authority.caller)
		if err != nil {
			o.tree.mu.Unlock()
			return teamTaskView{}, "", err
		}
		if authority.caller != nil {
			if parent.team == nil {
				o.tree.mu.Unlock()
				return teamTaskView{}, "", reject("permission_denied", "team membership is required")
			}
			ref := parent.team.Ref
			actor = &ref
		}
	}
	o.tree.mu.Unlock()
	store, err := o.teamStoreForRoot()
	if err != nil {
		return teamTaskView{}, "", err
	}
	return store.completeTask(id, actor)
}
