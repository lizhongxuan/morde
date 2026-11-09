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
	instructions := base
	if runtimeInstructions != "" {
		if strings.Contains(runtimeInstructions, base) {
			// DSH already installs the same Morde SOUL in its system prompt.
			// Keep the full runtime frame and avoid charging the model twice for it.
			instructions = runtimeInstructions
		} else {
			instructions += "\n\nAdditional trusted runtime instructions:\n" + runtimeInstructions
		}
	}
	return instructions + "\n\nMorde task execution policy:\n" + agentInstructions
}
