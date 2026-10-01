package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/urfave/cli/v3"
)

func TestHealthdStatusCommand_Flags(t *testing.T) {
	t.Parallel()

	cmd := newHealthdStatusCommand()
	assert.Equal(t, "status", cmd.Name)
	assert.NotEmpty(t, cmd.Flags)

	var portFlag *cli.IntFlag
	var formatFlag *cli.StringFlag
	var metricsFlag *cli.BoolFlag
	for _, f := range cmd.Flags {
		switch flag := f.(type) {
		case *cli.IntFlag:
			if flag.Name == "port" {
				portFlag = flag
			}
		case *cli.StringFlag:
			if flag.Name == "format" {
				formatFlag = flag
			}
		case *cli.BoolFlag:
			if flag.Name == "metrics" {
				metricsFlag = flag
			}
		}
	}
	assert.NotNil(t, metricsFlag, "expected --metrics flag to be present")
	if assert.NotNil(t, portFlag, "expected --port flag to be present") {
		assert.Equal(t, 9153, portFlag.Value)
	}
	if assert.NotNil(t, formatFlag, "expected --format flag to be present") {
		assert.Equal(t, "text", formatFlag.Value)
		assert.NoError(t, formatFlag.Action(t.Context(), cmd, "text"))
		assert.NoError(t, formatFlag.Action(t.Context(), cmd, "json"))
		assert.Error(t, formatFlag.Action(t.Context(), cmd, "yaml"))
		assert.Error(t, formatFlag.Action(t.Context(), cmd, "table"))
	}
}
