package agent

import (
	"os/exec"
	"strings"
)

// checkICU verifies the system can resolve libicuuc, the ICU core the .NET
// runtime bundled with CounterStrikeSharp dlopens. The failure without it is
// late and far from the cause: cssharp installs cleanly, the server boots,
// and the .NET runtime dies with "Couldn't find a valid ICU package" in the
// game journal — an operator met exactly that after a vanilla install.
//
// A warning, not an error: the plugin files are all in place and recorded, so
// an uninstall still works; the operator needs the instruction, not a failed
// install that hides what it already did.
func checkICU() error {
	out, err := exec.Command("ldconfig", "-p").Output()
	if err != nil {
		// No ldconfig (non-glibc): cannot judge either way, say nothing.
		return nil
	}
	if strings.Contains(string(out), "libicuuc.") {
		return nil
	}
	return warnf("CounterStrikeSharp will not load: libicuuc is missing — install your distro's ICU package (e.g. libicu76 on Debian 13, libicu74 on Ubuntu 24.04)")
}
