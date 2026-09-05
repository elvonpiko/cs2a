package agent

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// LaunchArgs are the parts of a CS2 launch line that decide whether the agent
// can talk to the server at all.
//
// A pre-existing server the installer adopted may bind a specific address, run
// on a non-default port, or omit -usercon entirely — and CS2 only opens its
// RCON listener when -usercon is present *and* rcon_password is set at boot.
// Guessing 127.0.0.1:27015 in that situation produced the single most
// confusing failure this panel had: "connection refused" with nothing to act on.
type LaunchArgs struct {
	// IP is the value of -ip ("" when unset, meaning all interfaces).
	IP string
	// Port is the value of -port (0 when unset).
	Port int
	// UserCon reports whether -usercon is on the command line.
	UserCon bool
	// Found reports whether a launch line was located at all.
	Found bool
	// Raw is the command line, for diagnostics.
	Raw string
}

// RCONHost returns the address the agent should dial for the parsed launch
// line. A wildcard bind (unset, 0.0.0.0, ::) is reachable on loopback, which is
// the safest choice; an explicit bind must be dialled on that exact address.
func (l LaunchArgs) RCONHost() string {
	switch l.IP {
	case "", "0.0.0.0", "::", "[::]", "*":
		return "127.0.0.1"
	}
	return l.IP
}

// reLaunchFlag matches "-ip 1.2.3.4", "-ip=1.2.3.4", "+port 27015" and the
// quoted forms systemd allows.
func launchFlagValue(argv []string, names ...string) string {
	match := func(arg string) bool {
		for _, n := range names {
			if arg == "-"+n || arg == "+"+n {
				return true
			}
		}
		return false
	}
	for i, arg := range argv {
		arg = strings.Trim(arg, `"'`)
		if match(arg) {
			if i+1 < len(argv) {
				return strings.Trim(argv[i+1], `"'`)
			}
			return ""
		}
		for _, n := range names {
			for _, prefix := range []string{"-" + n + "=", "+" + n + "="} {
				if v, ok := strings.CutPrefix(arg, prefix); ok {
					return strings.Trim(v, `"'`)
				}
			}
		}
	}
	return ""
}

// parseLaunchLine extracts the interesting flags from a CS2 command line.
func parseLaunchLine(line string) LaunchArgs {
	line = strings.TrimSpace(line)
	if line == "" {
		return LaunchArgs{}
	}
	argv := strings.Fields(line)
	out := LaunchArgs{Found: true, Raw: line}
	out.IP = launchFlagValue(argv, "ip")
	if v := launchFlagValue(argv, "port", "hostport"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			out.Port = n
		}
	}
	for _, arg := range argv {
		if strings.Trim(arg, `"'`) == "-usercon" {
			out.UserCon = true
			break
		}
	}
	return out
}

// reExecArgv pulls the argv[] list out of `systemctl show` ExecStart output:
//
//	ExecStart={ path=/opt/cs2/game/cs2.sh ; argv[]=/opt/cs2/game/cs2.sh -dedicated … ; ignore_errors=no ; … }
var reExecArgv = regexp.MustCompile(`argv\[\]=([^;}]*)`)

// parseExecStartProperty reads a launch line out of `systemctl show
// --property=ExecStart` output, tolerating both the structured form above and
// the plain `ExecStart=/path/args` of a raw unit file.
func parseExecStartProperty(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		value, ok := strings.CutPrefix(line, "ExecStart=")
		if !ok {
			// --value output has no key prefix.
			value = line
		}
		if value == "" {
			continue
		}
		if m := reExecArgv.FindStringSubmatch(value); m != nil {
			return strings.TrimSpace(m[1])
		}
		if strings.HasPrefix(value, "{") {
			continue // structured but no argv[] (shouldn't happen)
		}
		return value
	}
	return ""
}

// UnitLaunchArgs reads the game unit's effective launch line from systemd.
func (s *Systemd) UnitLaunchArgs() LaunchArgs {
	cmd := exec.Command(s.bin, "show", s.serviceName, "--property=ExecStart", "--no-pager")
	out, err := cmd.Output()
	if err != nil {
		return LaunchArgs{}
	}
	return parseLaunchLine(parseExecStartProperty(string(out)))
}

// UnitUser reports the user the game unit runs as ("" when unknown). The agent
// runs as root, so files it writes into the game tree must be handed to this
// user or the game cannot write them.
func (s *Systemd) UnitUser() string {
	cmd := exec.Command(s.bin, "show", s.serviceName, "--property=User", "--value", "--no-pager")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ActiveState reports systemd's ActiveState/SubState pair plus the unit's
// Result. "failed" units and units that never started need different operator
// text, and a plain is-active check cannot tell them apart.
func (s *Systemd) ActiveState() (active, sub, result string) {
	cmd := exec.Command(s.bin, "show", s.serviceName,
		"--property=ActiveState", "--property=SubState", "--property=Result", "--no-pager")
	out, err := cmd.Output()
	if err != nil {
		return "", "", ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "ActiveState":
			active = v
		case "SubState":
			sub = v
		case "Result":
			result = v
		}
	}
	return active, sub, result
}
