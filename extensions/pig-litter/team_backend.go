package piglitter

import "fmt"

type backendRequest string

type backendChoice struct {
	kind     backendRequest
	fallback string
}

const (
	backendEmbedded backendRequest = "embedded"
	backendAuto     backendRequest = "auto"
	backendTmux     backendRequest = "tmux"
	backendITerm    backendRequest = "iterm2"
)

func chooseTeamBackend(request backendRequest) (backendChoice, error) {
	switch request {
	case "", backendEmbedded:
		return backendChoice{kind: backendEmbedded}, nil
	case backendAuto:
		return backendChoice{kind: backendEmbedded, fallback: "native pane members are not available in this build"}, nil
	case backendTmux, backendITerm:
		return backendChoice{}, reject("unavailable", fmt.Sprintf("the %s teammate backend is not available in this build; choose embedded or auto", request))
	default:
		return backendChoice{}, reject("rejected", "backend must be embedded, tmux, iterm2, or auto")
	}
}
