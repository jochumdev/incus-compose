package testlib

import (
	"regexp"
	"strings"
)

var (
	ipv4Regex         = regexp.MustCompile(`\d+\.\d+\.\d+\.\d+`)
	ipv6Regex         = regexp.MustCompile(`(?:[0-9a-fA-F]{0,4}:){2,7}[0-9a-fA-F]{0,4}`)
	healthdImageRegex = regexp.MustCompile("ic-healthd:[0-9a-z.-]+")
	healthRegex       = regexp.MustCompile(`"health": "[a-zA-Z]+",`)
)

// Strip replaces what changes between runs - addresses and the healthd image
// tag - so output can be snapshotted. The trailing newline goes too, since
// cupaloy adds one back.
func Strip(out string) string {
	out = ipv4Regex.ReplaceAllString(out, "-stripped-")
	out = ipv6Regex.ReplaceAllString(out, "-stripped-")
	out = healthdImageRegex.ReplaceAllString(out, "ic-healthd:-stripped-")

	return strings.Trim(out, "\n")
}

// StripHealth is Strip, and also the reported health status. Use it where the
// status is still settling and the test is not about it.
func StripHealth(out string) string {
	return Strip(healthRegex.ReplaceAllString(out, `"health": "-stripped-",`))
}

// StripIPv6Lines drops every line carrying an IPv6 address.
func StripIPv6Lines(out string) string {
	kept := []string{}
	for line := range strings.SplitSeq(out, "\n") {
		if !ipv6Regex.MatchString(line) {
			kept = append(kept, line)
		}
	}

	return strings.Join(kept, "\n")
}
