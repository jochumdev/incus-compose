package testlib

import (
	"os"
	"strings"
)

// ProjectName makes an Incus project name out of anything, so a test can pass
// t.Name() and get one back.
func ProjectName(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, "/", "-"))
}

// Args builds the incus-compose arguments for a project, ahead of whatever the
// caller runs them with.
func Args(project string, args ...string) []string {
	return append([]string{"--debug", "--project-name", ProjectName(project)}, args...)
}

func KeepTestData() bool {
	_, keep := os.LookupEnv("INCUS_COMPOSE_TEST_KEEP")
	return keep
}
