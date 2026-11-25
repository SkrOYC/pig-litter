//go:build teammate_discovery

package piglitter

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"testing"
)

func TestNativeMemberDiscoveryFromLoadedSourceAndPiglet(t *testing.T) {
	expected := os.Getenv("TEAM_EXPECT_HELPER")
	if expected == "" {
		t.Fatal("TEAM_EXPECT_HELPER is required")
	}
	for _, item := range []struct {
		name, path string
		selected   bool
	}{
		{name: "source", path: os.Getenv("TEAM_SOURCE_RPC"), selected: true},
		{name: "piglet", path: os.Getenv("TEAM_PIGLET_RPC"), selected: true},
		{name: "standalone", path: os.Getenv("TEAM_STANDALONE_RPC"), selected: true},
		{name: "unselected", path: os.Getenv("TEAM_UNSELECTED_RPC")},
	} {
		t.Run(item.name, func(t *testing.T) {
			file, err := os.Open(item.path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			found := 0
			scanner := bufio.NewScanner(file)
			scanner.Buffer(make([]byte, 4096), 1024*1024)
			for scanner.Scan() {
				var frame struct {
					ID   string `json:"id"`
					Data struct {
						Commands []struct {
							Name       string `json:"name"`
							SourceInfo struct {
								Path string `json:"path"`
							} `json:"sourceInfo"`
						} `json:"commands"`
					} `json:"data"`
				}
				if json.Unmarshal(scanner.Bytes(), &frame) != nil || frame.ID != "commands" {
					continue
				}
				for _, command := range frame.Data.Commands {
					if command.Name != "pig-litter" {
						continue
					}
					found++
					actual, err := nativeMemberExecutable(context.Background(), command.SourceInfo.Path)
					if err != nil || actual != expected {
						t.Fatalf("discovery from %s = %q, %v", command.SourceInfo.Path, actual, err)
					}
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			if item.selected && found != 1 || !item.selected && found != 0 {
				t.Fatalf("selected command count = %d", found)
			}
		})
	}
}
