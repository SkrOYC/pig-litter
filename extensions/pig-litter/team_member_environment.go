package piglitter

import (
	"errors"
	"os"
	"strings"
)

func nativeTerminalVariable(name string) bool {
	return name == "TERM" || name == "TERM_SESSION_ID" || name == "TERM_PROGRAM" || name == "TERM_PROGRAM_VERSION" || name == "COLORTERM" || name == "COLORFGBG" || name == "WINDOWID" || name == "COLUMNS" || name == "LINES" || strings.HasPrefix(name, "ITERM_") || name == "TMUX" || name == "TMUX_PANE"
}

func nativeRuntimeVariable(name string) bool {
	return name != "" && !strings.ContainsAny(name, "=\x00") && !nativeTerminalVariable(name) && !strings.HasPrefix(name, "PIG_EXT_") && !strings.HasPrefix(name, "PIG_HARNESS_")
}

func nativeMemberEnvironment(environ []string) map[string]string {
	result := make(map[string]string)
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if ok && nativeRuntimeVariable(name) {
			result[name] = value
		}
	}
	return result
}

func applyNativeMemberEnvironment(environment map[string]string) error {
	terminal := make(map[string]string)
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if ok && nativeTerminalVariable(name) && name != "TMUX" && name != "TMUX_PANE" {
			terminal[name] = value
		}
	}
	for name, value := range environment {
		if !nativeRuntimeVariable(name) || strings.ContainsRune(value, 0) {
			return errors.New("native member environment is invalid")
		}
	}
	os.Clearenv()
	for name, value := range environment {
		if err := os.Setenv(name, value); err != nil {
			return errors.New("native member environment is invalid")
		}
	}
	for name, value := range terminal {
		if err := os.Setenv(name, value); err != nil {
			return errors.New("native terminal environment is invalid")
		}
	}
	return nil
}
