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
