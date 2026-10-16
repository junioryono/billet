package deploy_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPackageKeepsPrivilegedStateOutsideTheServiceAccountsAuthority(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker is required to execute the package installer on Linux")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", "ubuntu:24.04").Run(); err != nil {
		t.Skipf("a local ubuntu:24.04 image and Docker daemon are required: %v", err)
	}
	script, err := filepath.Abs("scripts/postinstall.sh")
	if err != nil {
		t.Fatal(err)
	}
	name := "billet-package-permissions-" + filepath.Base(filepath.Dir(t.TempDir()))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer stop()
		out, err := exec.CommandContext(cleanup, "docker", "rm", "-f", name).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "No such container:") {
			t.Errorf("remove the package-test container: %v: %s", err, out)
		}
	})
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--pull=never", "--network=none",
		"--name", name, "--mount", "type=bind,src="+script+",dst=/postinstall.sh,readonly",
		"-i", "ubuntu:24.04", "sh", "-eu")
	cmd.Stdin = strings.NewReader(`
sh /postinstall.sh
mkdir -m 700 /var/lib/billet/upgrades /var/lib/billet/server
chown billet:billet /var/lib/billet/server
test "$(stat -c '%U:%G %a' /var/lib/billet)" = 'root:root 755'
runuser -u billet -- touch /var/lib/billet/server/ledger-fixture
if runuser -u billet -- mv /var/lib/billet/upgrades /var/lib/billet/replaced; then
    echo 'service account replaced privileged state' >&2
    exit 1
fi
if runuser -u billet -- mkdir /var/lib/billet/new-root-state; then
    echo 'service account planted privileged state' >&2
    exit 1
fi
sh /postinstall.sh
test "$(stat -c '%U:%G %a' /var/lib/billet)" = 'root:root 755'
test "$(stat -c '%U:%G %a' /var/lib/billet/server)" = 'billet:billet 700'
test -f /var/lib/billet/server/ledger-fixture
`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package privilege boundary: %v\n%s", err, out)
	}
}

// TestPackageSeedsTheNeedrestartExclusionOnlyWhereNeedrestartIs executes the
// postinstall in a container: nothing appears on a host without needrestart,
// the shipped drop-in appears where /etc/needrestart exists (and needrestart's
// own loader, `defined(do $fn)` under perl, accepts it and excludes billet's
// units and nothing else), a reinstall leaves it alone and leaves no temporary
// file, and an operator's file or symlink at the name is never replaced.
func TestPackageSeedsTheNeedrestartExclusionOnlyWhereNeedrestartIs(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker is required to execute the package installer on Linux")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", "ubuntu:24.04").Run(); err != nil {
		t.Skipf("a local ubuntu:24.04 image and Docker daemon are required: %v", err)
	}
	script, err := filepath.Abs("scripts/postinstall.sh")
	if err != nil {
		t.Fatal(err)
	}
	dropIn, err := filepath.Abs("needrestart.conf")
	if err != nil {
		t.Fatal(err)
	}
	name := "billet-package-needrestart-" + filepath.Base(filepath.Dir(t.TempDir()))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer stop()
		out, err := exec.CommandContext(cleanup, "docker", "rm", "-f", name).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "No such container:") {
			t.Errorf("remove the package-test container: %v: %s", err, out)
		}
	})
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--pull=never", "--network=none",
		"--name", name,
		"--mount", "type=bind,src="+script+",dst=/postinstall.sh,readonly",
		"--mount", "type=bind,src="+dropIn+",dst=/usr/share/billet/needrestart.conf,readonly",
		"-i", "ubuntu:24.04", "sh", "-eu")
	cmd.Stdin = strings.NewReader(`
sh /postinstall.sh
if [ -e /etc/needrestart ]; then
    echo 'the package created /etc/needrestart on a host without needrestart' >&2
    exit 1
fi

mkdir /etc/needrestart
sh /postinstall.sh
cmp /usr/share/billet/needrestart.conf /etc/needrestart/conf.d/90-billet.conf
test "$(stat -c '%U:%G %a' /etc/needrestart/conf.d/90-billet.conf)" = 'root:root 644'
test "$(stat -c '%U:%G %a' /etc/needrestart/conf.d)" = 'root:root 755'
perl -e '
    our %nrconf;
    die "needrestart would refuse it: $@ $!" unless defined(do "/etc/needrestart/conf.d/90-billet.conf");
    my @res = keys %{$nrconf{override_rc}};
    die "override_rc has @res" unless @res == 1;
    for my $unit ("billet-node.service", "billet-server.service", "billet-dnsmasq\@billet0.service") {
        die "$unit is not excluded" unless $unit =~ /$res[0]/ && $nrconf{override_rc}{$res[0]} == 0;
    }
    for my $unit ("ssh.service", "docker.service", "my-billet-node.service") {
        die "$unit is excluded" if $unit =~ /$res[0]/;
    }
'
inode=$(stat -c %i /etc/needrestart/conf.d/90-billet.conf)
sh /postinstall.sh
test "$(stat -c %i /etc/needrestart/conf.d/90-billet.conf)" = "${inode}"
test "$(ls -A /etc/needrestart/conf.d)" = '90-billet.conf'

printf 'operator\n' >/etc/needrestart/conf.d/90-billet.conf
sh /postinstall.sh
test "$(cat /etc/needrestart/conf.d/90-billet.conf)" = 'operator'

rm /etc/needrestart/conf.d/90-billet.conf
ln -s /nowhere /etc/needrestart/conf.d/90-billet.conf
sh /postinstall.sh
test "$(readlink /etc/needrestart/conf.d/90-billet.conf)" = '/nowhere'
test ! -e /nowhere
rm /etc/needrestart/conf.d/90-billet.conf

# A FAILURE IS REPORTED AND NEVER ENDS THE INSTALL, and a file that appears at
# the name between the check and the publication is never overwritten. The
# mktemp shim acts only on the drop-in's temporary name: it fails, or it makes
# the temporary file and then plants an operator's file at the destination.
mkdir /shim
cat >/shim/mktemp <<'SHIM'
#!/bin/sh
case "$1" in
    /etc/needrestart/conf.d/90-billet.conf.*) ;;
    *) exec /usr/bin/mktemp "$@" ;;
esac
if [ "${SHIM_MODE}" = fail ]; then
    exit 1
fi
made=$(/usr/bin/mktemp "$@") || exit 1
printf 'raced\n' >/etc/needrestart/conf.d/90-billet.conf
printf '%s\n' "${made}"
SHIM
chmod 0755 /shim/mktemp

SHIM_MODE=fail PATH="/shim:${PATH}" sh /postinstall.sh 2>/failed.err
grep -F '/etc/needrestart/conf.d/90-billet.conf could not be installed' /failed.err
test -z "$(ls -A /etc/needrestart/conf.d)"

SHIM_MODE=race PATH="/shim:${PATH}" sh /postinstall.sh 2>/raced.err
grep -F '/etc/needrestart/conf.d/90-billet.conf could not be installed' /raced.err
test "$(cat /etc/needrestart/conf.d/90-billet.conf)" = 'raced'
test "$(ls -A /etc/needrestart/conf.d)" = '90-billet.conf'
rm /etc/needrestart/conf.d/90-billet.conf

rmdir /etc/needrestart/conf.d
touch /etc/needrestart/conf.d
sh /postinstall.sh 2>/blocked.err
grep -F '/etc/needrestart/conf.d/90-billet.conf could not be installed' /blocked.err
test -f /etc/needrestart/conf.d
test ! -s /etc/needrestart/conf.d
`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package needrestart exclusion: %v\n%s", err, out)
	}
}

// TestPackageCreatesTheHostLockDirectoryWhereSystemdRuns executes the
// postinstall in a container: where systemd runs, it reloads systemd and then
// asks systemd-tmpfiles to create the entry the package installs, exactly once
// and by its path, before the timers are armed; a
// failure is said and never ends the install, because the next boot creates the
// directory; and where systemd is not running (an image build, a chroot) it
// asks nothing. Stand-ins answer for systemctl and systemd-tmpfiles, whose real
// effect on the entry is measured against systemd in
// internal/lifeops's real-systemd test.
func TestPackageCreatesTheHostLockDirectoryWhereSystemdRuns(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker is required to execute the package installer on Linux")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", "ubuntu:24.04").Run(); err != nil {
		t.Skipf("a local ubuntu:24.04 image and Docker daemon are required: %v", err)
	}
	script, err := filepath.Abs("scripts/postinstall.sh")
	if err != nil {
		t.Fatal(err)
	}
	name := "billet-package-tmpfiles-" + filepath.Base(filepath.Dir(t.TempDir()))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer stop()
		out, err := exec.CommandContext(cleanup, "docker", "rm", "-f", name).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "No such container:") {
			t.Errorf("remove the package-test container: %v: %s", err, out)
		}
	})
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--pull=never", "--network=none",
		"--name", name, "--mount", "type=bind,src="+script+",dst=/postinstall.sh,readonly",
		"-i", "ubuntu:24.04", "sh", "-eu")
	cmd.Stdin = strings.NewReader(`
mkdir /shim
printf '#!/bin/sh\nprintf "systemctl %%s\\n" "$*" >>/calls\nexit 0\n' >/shim/systemctl
printf '#!/bin/sh\nprintf "%%s\\n" "$*" >>/tmpfiles.argv\nprintf "tmpfiles\\n" >>/calls\nexit "${TMPFILES_EXIT:-0}"\n' >/shim/systemd-tmpfiles
chmod 0755 /shim/systemctl /shim/systemd-tmpfiles

PATH="/shim:${PATH}" sh /postinstall.sh
if [ -e /tmpfiles.argv ]; then
    echo 'the package asked systemd-tmpfiles for something where systemd is not running' >&2
    exit 1
fi

mkdir -p /run/systemd/system
# A billet that answers the authority status open, so the unit decisions run
# and arm the timers.
printf '#!/bin/sh\necho "{\\"closed\\":false}"\n' >/usr/bin/billet
chmod 0755 /usr/bin/billet
rm -f /calls
PATH="/shim:${PATH}" sh /postinstall.sh
test "$(cat /tmpfiles.argv)" = '--create /usr/lib/tmpfiles.d/billet.conf'

# AFTER A RELOAD, so a stop under the old loaded unit no longer removes it,
# and BEFORE THE TIMERS ARE ARMED: the image refresh requires the directory.
reload_at=$(grep -n '^systemctl daemon-reload$' /calls | head -n 1 | cut -d: -f1)
tmpfiles_at=$(grep -n '^tmpfiles$' /calls | cut -d: -f1)
timer_at=$(grep -n 'enable --now billet-images-refresh.timer' /calls | cut -d: -f1)
if [ -z "${reload_at}" ] || [ -z "${tmpfiles_at}" ] || [ -z "${timer_at}" ] ||
    [ "${reload_at}" -ge "${tmpfiles_at}" ] || [ "${tmpfiles_at}" -ge "${timer_at}" ]; then
    echo "the directory was not made between a reload and the image refresh's timer:" >&2
    cat /calls >&2
    exit 1
fi

rm /tmpfiles.argv
TMPFILES_EXIT=1 PATH="/shim:${PATH}" sh /postinstall.sh 2>/tmpfiles.err
test "$(cat /tmpfiles.argv)" = '--create /usr/lib/tmpfiles.d/billet.conf'
grep -F '/run/billet/locks could not be created now' /tmpfiles.err
`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package host lock directory: %v\n%s", err, out)
	}
}
