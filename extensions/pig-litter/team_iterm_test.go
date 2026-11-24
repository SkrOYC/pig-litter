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
