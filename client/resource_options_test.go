package client

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestOptionsIncusTimeout(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		want    int
	}{
		{name: "unset asks for the daemon default", want: -1},
		{name: "whole seconds pass through", timeout: 30 * time.Second, want: 30},
		{name: "a sub-second timeout is a second, not a kill", timeout: 500 * time.Millisecond, want: 1},
		{name: "a nanosecond is still a second", timeout: time.Nanosecond, want: 1},
		{name: "fractions truncate towards the second below", timeout: 1500 * time.Millisecond, want: 1},
		{name: "minutes are seconds", timeout: 2 * time.Minute, want: 120},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, NewOptions(OptionTimeout(tt.timeout)).incusTimeout())
		})
	}
}
