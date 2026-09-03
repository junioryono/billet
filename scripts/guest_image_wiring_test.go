package scripts_test

import (
	"strings"
	"testing"
)

// TestTheBuildActuallyCallsItsGuards is the class of test whose absence let a
// build-breaking ordering bug through.
//
// EVERY OTHER TEST HERE DRIVES A HELPER DIRECTLY, which proves the helper works
// and says nothing about whether the build calls it, or calls it at the right
// moment. Deleting `verify_toolset` from main, or moving the contract read after
// the unmount, leaves all of them green — and the second of those made every
// build fail, found by review rather than by the suite.
//
// STRUCTURAL, AND DELIBERATELY SO. Running the real build needs root, debootstrap
// and an hour. What can be checked cheaply is that the calls exist and are in the
// only order that works, which is exactly what went wrong.
func TestTheBuildActuallyCallsItsGuards(t *testing.T) {
	t.Parallel()

	source := readBuildScript(t)

	main := mainBody(t, source)

	// PRESENT AT ALL. A guard nothing calls is decoration.
	for _, call := range []struct{ name, why string }{
		{"verify_toolset", "the declaration would not be checked against its pin, so an " +
			"edited toolset would decide what every image contains"},
		{"clear_stale_mount", "a workspace left mounted by a killed build would be deleted " +
			"through, erasing the filesystem rather than the directory"},
		{"flock", "two builds could share a workspace, and each begins by unmounting and " +
			"deleting it"},
	} {
		if !strings.Contains(main, call.name) {
			t.Errorf("main never calls %s: %s", call.name, call.why)
		}
	}
}

// TestEverythingThatReadsTheImageRunsBeforeTheUnmount is the specific ordering
// that broke.
//
// The build writes THROUGH a mountpoint, so every step that describes the image
// reads files inside it. After unmount_rootfs that path is an empty host
// directory: the read finds nothing and the build stops with a message blaming
// the agent. Moving filesystem creation to the start of the build turned every
// read of the finished tree into a read through a mountpoint, and the contract
// read was the one that did not move with it.
func TestEverythingThatReadsTheImageRunsBeforeTheUnmount(t *testing.T) {
	t.Parallel()

	main := mainBody(t, readBuildScript(t))

	unmountAt := strings.Index(main, "\n\tunmount_rootfs\n")
	if unmountAt < 0 {
		t.Fatal("main never unmounts the root filesystem, so the image is never finalized")
	}

	for _, tc := range []struct{ call, why string }{
		{"read_guest_contract", "the guest contract is read out of the agent INSIDE the " +
			"image; after the unmount there is no agent to read"},
		{"install_toolcache", "the toolcache is written into the image; after the unmount " +
			"it would land on the build host"},
	} {
		at := strings.Index(main, tc.call)

		switch {
		case at < 0:
			t.Errorf("main never calls %s", tc.call)
		case at > unmountAt:
			t.Errorf("%s runs AFTER unmount_rootfs: %s", tc.call, tc.why)
		}
	}
}

// TestTheImageEnvironmentFileIsCreatedOnceAndBeforeItIsAppendedTo guards an
// ordering that silently discarded everything the JDK step wrote.
//
// THE JDK STEP APPENDS A JAVA_HOME PER VERSION. When the file was created after
// the toolcache ran, with `>`, that create silently discarded every one of them —
// five JDKs installed and unfindable, which is precisely the failure the toolcache
// section warns about one directory over. Nothing about a running image would
// have said so: setup-java would simply install its own JDK and the job would
// pass, slowly.
func TestTheImageEnvironmentFileIsCreatedOnceAndBeforeItIsAppendedTo(t *testing.T) {
	t.Parallel()

	source := readBuildScript(t)

	// EXACTLY ONE TRUNCATING WRITE. A second one anywhere discards whatever the
	// first and every appender had put there, and which of them wins depends on
	// an order nothing states.
	//
	// `>>` CONTAINS `>`, so counting the truncating form has to subtract the
	// appending one or every append reads as a create. The first version of this
	// check did not, reported four creates against one, and would have sent a
	// reader looking for three writes that do not exist.
	creates := strings.Count(source, `/etc/billet-image-env" <<`) +
		strings.Count(source, `>"$rootfs/etc/billet-image-env"`) -
		strings.Count(source, `>>"$rootfs/etc/billet-image-env"`)
	if creates != 1 {
		t.Errorf("the image environment file is created %d times; every create truncates, so "+
			"a second one silently discards the variables written before it", creates)
	}

	main := mainBody(t, source)

	createAt := strings.Index(main, `/etc/billet-image-env" <<`)
	toolcacheAt := strings.Index(main, "install_toolcache")

	switch {
	case createAt < 0:
		t.Fatal("main never creates /etc/billet-image-env, so a job sees none of the " +
			"variables a hosted runner exports")
	case toolcacheAt < 0:
		t.Fatal("main never installs the toolcache")
	case createAt > toolcacheAt:
		t.Error("the image environment file is created AFTER install_toolcache, which " +
			"appends to it — so every JAVA_HOME the JDKs wrote is discarded and the JDKs " +
			"are installed and unfindable")
	}
}

// mainBody returns the body of the build script's main function.
func mainBody(t *testing.T, source string) string {
	t.Helper()

	start := strings.Index(source, "\nmain() {\n")
	if start < 0 {
		t.Fatal("build-guest-image.sh has no main function")
	}

	// NOT THE FIRST `\n}\n`. main embeds heredocs whose content has a closing
	// brace at column zero — /etc/docker/daemon.json is JSON — so searching for
	// one truncates the body partway through and every ordering check below it
	// silently sees nothing. The first attempt at this test did exactly that and
	// reported "main never unmounts", which is the vacuous-extraction failure
	// this project has hit before with scripted edits.
	//
	// The publish helpers are defined AFTER main, so the body ends at the last
	// function-closing brace before them.
	const nextFunction = "\ntake_publish_lock() {"

	limit := strings.Index(source, nextFunction)
	if limit < 0 {
		t.Fatal("build-guest-image.sh no longer defines take_publish_lock after main; this " +
			"test uses it to find where main ends")
	}

	end := strings.LastIndex(source[start:limit], "\n}\n")
	if end < 0 {
		t.Fatal("could not find the end of main")
	}

	return source[start : start+end]
}
