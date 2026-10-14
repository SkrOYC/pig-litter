package piglitter

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
)

func nativeTestManager(t *testing.T, directory, id string) *coding.SessionManager {
	t.Helper()
	trusted := true
	services, err := coding.NewServices(coding.ServicesOptions{CWD: directory, AgentDir: directory, ProjectTrusted: &trusted})
	if err != nil {
		t.Fatal(err)
	}
	defer services.Close()
	runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services, AbortContext: context.Background()})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	session, err := runtime.New(coding.SessionStartOptions{SessionID: id, SessionDir: filepath.Join(directory, "sessions"), ResourceLoader: coding.NoResources, AllowedTools: map[string]struct{}{}})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	manager := session.SessionManager()
	if _, err := manager.AppendMessage(agent.AgentMessage{User: &agent.UserMessage{Role: "user", Content: ai.UserContentBlocks{ai.TextContent{Text: "retained marker"}}}}); err != nil {
		t.Fatal(err)
	}
	return manager
}
