package ec2

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// imagePlatformWindows is what DescribeImages says of a Windows AMI, and all it says:
// the field is absent on every other image.
const imagePlatformWindows = "windows"

// checkGuestOS refuses a launch whose guest OS and AMI disagree, in either direction,
// before anything is sent.
//
// THE LEASE SAYS WHAT THE JOB NEEDS AND THE IMAGE SAYS WHAT IT IS, and neither stands
// in for the other. Linux's boot script on a Windows image is text EC2Launch does not
// run, and `<powershell>` on a Linux image is text cloud-init does not: either boots,
// reports success, registers nothing and holds a lease until something reaps it.
func checkGuestOS(spec provider.Spec, layout imageLayout) error {
	if wants := spec.GuestOS == config.GuestWindows; wants == layout.windows() {
		return nil
	}

	if layout.windows() {
		return fmt.Errorf("ec2: %s is a %s lease and its image %s is a Windows AMI, so its boot "+
			"script would not run there; a Windows tier sets guest_os: windows, and any other "+
			"names an AMI of its own", spec.Name, guestOSName(spec.GuestOS), spec.Image)
	}

	return fmt.Errorf("ec2: %s is a Windows lease and its image %s is not a Windows AMI, so its "+
		"boot script would not run there; build one with `billet ami build --os windows`",
		spec.Name, spec.Image)
}

// guestOSName is how an error names a lease's guest OS, whose zero value is Linux.
func guestOSName(os config.GuestOS) string {
	if os == "" {
		return string(config.GuestLinux)
	}

	return string(os)
}

// psQuote renders a value as a single-quoted PowerShell word.
//
// PRINTABLE ASCII OR NOTHING, and that is the whole of the argument. Inside single
// quotes PowerShell expands nothing, and it reads a doubled quote as one; but it also
// takes the typographic quotes U+2018 to U+201B as single quotes, and EC2Launch's
// script reaches PowerShell 5.1 with no byte-order mark, which reads it in the machine's
// ANSI code page, so a UTF-8 trail byte can be read as one of those. No registration, AMI
// path or runner argument needs a byte outside the range, and a refused value is a
// refused launch rather than a boot script that means something else.
//
// `<` AND `>` ARE REFUSED WITH THEM: EC2Launch finds the script between its tags in the
// raw text, and a value holding `</powershell>` would end it there, whatever the quotes.
func psQuote(value string) (string, error) {
	for i := range len(value) {
		c := value[i]
		if c < 0x20 || c > 0x7e {
			return "", errors.New("the value holds a byte that is not printable ASCII")
		}

		if c == '<' || c == '>' {
			return "", errors.New("the value holds an angle bracket, which could end the " +
				"boot script's tag")
		}
	}

	return "'" + strings.ReplaceAll(value, "'", "''") + "'", nil
}

// windowsUserData is the boot script of a job on a Windows AMI.
//
// THE REGISTRATION IS AN ENVIRONMENT VARIABLE OF THE SCRIPT'S OWN PROCESS, as on Linux,
// and the trade is ADR-002's: the instance holds one job, is destroyed with it, and the
// registration is single-use and consumed before any step runs. The entry point takes
// it from there in no process's argument list (billet-runner.ps1).
//
// A CACHE FIELD IS REFUSED, NOT DROPPED. The node prepares no bearer for a Windows lease,
// so one arriving here is a caller believing a cache exists; dropping it would be a job
// that reads cold for a reason nothing says (ADR-023).
func windowsUserData(spec provider.Spec, launchEpochNS int64) (string, error) {
	switch {
	case spec.CacheEndpoint != "", spec.CacheToken != "", spec.ActionsProxy != "", spec.NpmProxy != "",
		spec.ActionsCAPEM != "", spec.GuestMonitor, len(spec.GuestCaches) > 0:
		return "", fmt.Errorf("ec2: %s asks a Windows guest for billet's caches or its in-VM "+
			"agent, whose clients are in a Linux guest", spec.Name)
	}

	jit, err := psQuote(spec.JITConfig)
	if err != nil {
		return "", fmt.Errorf("ec2: the JIT config for %s cannot be carried in a boot script: %w",
			spec.Name, err)
	}

	words := make([]string, 0, len(spec.Command))

	for _, arg := range spec.Command {
		word, err := psQuote(arg)
		if err != nil {
			return "", fmt.Errorf("ec2: a command argument of %s cannot be carried in a boot "+
				"script: %w", spec.Name, err)
		}

		words = append(words, word)
	}

	var b strings.Builder

	b.WriteString("<powershell>\n$ErrorActionPreference = 'Stop'\n")
	b.WriteString("$env:BILLET_LAUNCH_EPOCH_NS = '" + strconv.FormatInt(launchEpochNS, 10) + "'\n")
	b.WriteString("$env:" + jitEnvVar + " = " + jit + "\n")
	b.WriteString("& " + strings.Join(words, " ") + "\n")
	b.WriteString("exit $LASTEXITCODE\n</powershell>\n")

	return b.String(), nil
}
