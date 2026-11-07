package app

import (
	_ "embed"
	"strings"
)

//go:embed SOUL.md
var mordeSoulFile string

func mordeSoulInstructions(runtimeInstructions string) string {
	base := strings.TrimSpace(mordeSoulFile)
	runtimeInstructions = strings.TrimSpace(runtimeInstructions)
	if runtimeInstructions == "" {
		return base
	}
	return base + "\n\nAdditional trusted runtime instructions:\n" + runtimeInstructions
}
