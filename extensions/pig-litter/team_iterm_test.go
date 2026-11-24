package piglitter

import "testing"

func TestITermSessionIdentity(t *testing.T) {
	value := "41\n48187B77-445F-4377-8CB5-3257430A0793\n/dev/ttys002"
	window, session, tty, err := parseITermSession(value)
	if err != nil || window != "41" || session != "48187B77-445F-4377-8CB5-3257430A0793" || tty != "/dev/ttys002" {
		t.Fatalf("session identity = %q %q %q, %v", window, session, tty, err)
	}
	for _, value := range []string{"41", "41\n48187B77-445F-4377-8CB5-3257430A0793\n/dev/ttys002\nextra", "41\nnot-a-session\n/dev/ttys002"} {
		if _, _, _, err := parseITermSession(value); err == nil {
			t.Fatalf("accepted incomplete session identity %q", value)
		}
	}
}

func TestITermProcessDiscoveryUsesUIDAndExecutableIdentity(t *testing.T) {
	output := "501 1743 /Applications/iTerm.app/Contents/MacOS/iTerm2\n" +
		"501 1900 /Users/test/Apps/Custom iTerm.app/Contents/MacOS/iTerm2 -psn_0_42\n" +
		"501 2000 /Applications/iTerm.app/Contents/MacOS/iTerm2Helper\n" +
		"502 2100 /Applications/iTerm.app/Contents/MacOS/iTerm2\n" +
		"501 2200 /bin/sh -c /Applications/iTerm.app/Contents/MacOS/iTerm2\n"
	candidates := itermProcessCandidates(output, 501)
	if len(candidates) != 3 || candidates[0] != 1743 || candidates[1] != 1900 || candidates[2] != 2200 {
		t.Fatalf("candidate process IDs = %v", candidates)
	}
	for path, want := range map[string]bool{
		"/Applications/iTerm.app/Contents/MacOS/iTerm2":             true,
		"/Users/test/Apps/Custom iTerm.app/Contents/MacOS/iTerm2": true,
		"/Applications/iTerm.app/Contents/MacOS/iTerm2Helper":       false,
		"/Applications/not-a-bundle/Contents/MacOS/iTerm2":          false,
		"/bin/sh": false,
	} {
		if got := itermApplicationExecutable(path); got != want {
			t.Errorf("executable identity for %q = %v, want %v", path, got, want)
		}
	}
}
