package agents

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"
)

// compatible is nil when an Agent of version agent may talk to a Control Plane of version cp (Q41): the same minor
// version, or one behind, so the Control Plane can be upgraded first and the Agents after. Otherwise it says what to
// upgrade. Development builds without a version are let through.
func compatible(cp, agent string) error {
	if !semver.IsValid(cp) || !semver.IsValid(agent) {
		return nil
	}
	cpMajor, cpMinor := majorMinor(cp)
	major, minor := majorMinor(agent)
	switch {
	case major == cpMajor && (minor == cpMinor || minor == cpMinor-1):
		return nil
	case major < cpMajor || (major == cpMajor && minor < cpMinor):
		return fmt.Errorf("Agent %s is too old for this Control Plane (%s), which serves Agents from v%d.%d on: upgrade "+
			"the Agent; its Applications keep running meanwhile", agent, cp, cpMajor, max(cpMinor-1, 0))
	default:
		return fmt.Errorf("Agent %s is newer than this Control Plane (%s): upgrade the Control Plane to %s or later first; "+
			"the Agent's Applications keep running meanwhile", agent, cp, semver.MajorMinor(agent))
	}
}

func majorMinor(v string) (int, int) {
	major, minor, _ := strings.Cut(strings.TrimPrefix(semver.MajorMinor(v), "v"), ".")
	a, _ := strconv.Atoi(major)
	b, _ := strconv.Atoi(minor)
	return a, b
}
