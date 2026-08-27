package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSplitCopyPath(t *testing.T) {
	services := []string{"db", "web"}

	tests := []struct {
		name    string
		arg     string
		service string
		path    string
	}{
		{name: "a service and an absolute path", arg: "web:/etc/nginx", service: "web", path: "/etc/nginx"},
		{name: "a service and a relative path", arg: "web:etc/nginx", service: "web", path: "etc/nginx"},
		{name: "a path holding a colon is local", arg: "./a:b", path: "./a:b"},
		{name: "an unknown name is a local path", arg: "notaservice:/etc", path: "notaservice:/etc"},
		{name: "a windows drive is local", arg: `C:\data`, path: `C:\data`},
		{name: "stdin is local", arg: "-", path: "-"},
		{name: "a plain local path", arg: "./html", path: "./html"},
		{name: "a service with an empty path", arg: "db:", service: "db"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitCopyPath(tt.arg, services)

			assert.Equal(t, tt.service, got.service)
			assert.Equal(t, tt.path, got.path)
		})
	}
}

func TestInstancePath(t *testing.T) {
	tests := []struct {
		name     string
		instance string
		path     string
		want     string
	}{
		{name: "an absolute path loses its leading slash", instance: "web-1", path: "/etc/hosts", want: "web-1/etc/hosts"},
		{name: "a relative path is joined as is", instance: "web-1", path: "etc/hosts", want: "web-1/etc/hosts"},
		{name: "the root is the instance itself", instance: "web-1", path: "/", want: "web-1/"},
		{name: "a trailing slash survives, it asks for a directory", instance: "db-2", path: "/var/lib/", want: "db-2/var/lib/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, instancePath(tt.instance, tt.path))
		})
	}
}
