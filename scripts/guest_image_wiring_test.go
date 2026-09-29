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
		{"run_runner_images_build", "GitHub's build is run in the image; after the unmount " +
			"there is no image to boot"},
		{"write_image_env", "the job environment is read out of the image's " +
			"/etc/environment; after the unmount there is none to read"},
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

// TestTheImageEnvironmentIsWrittenOnceAfterGitHubsBuild guards where the job's
// environment comes from.
//
// GitHub's scripts write the hosted environment to /etc/environment as they
// install, and the agent hands a job exactly what /etc/billet-image-env names. So
// that file is written once, by write_image_env, and only after the build that
// wrote /etc/environment: before it, every JAVA_HOME and GOROOT a later step
// sets would be missing, and a second writer would decide the result by order.
func TestTheImageEnvironmentIsWrittenOnceAfterGitHubsBuild(t *testing.T) {
	t.Parallel()

	source := readBuildScript(t)

	// ONE WRITER, to a temporary file renamed into place, so no reader sees half.
	if writes := strings.Count(source, `>"$rootfs/etc/billet-image-env`); writes != 1 {
		t.Errorf("the image environment file is written %d times; it must be written once, "+
			"by write_image_env", writes)
	}

	main := mainBody(t, source)

	buildAt := strings.Index(main, "run_runner_images_build")
	envAt := strings.Index(main, "write_image_env")

	switch {
	case buildAt < 0:
		t.Fatal("main never runs GitHub's build")
	case envAt < 0:
		t.Fatal("main never writes /etc/billet-image-env, so a job sees none of the " +
			"variables a hosted runner exports")
	case envAt < buildAt:
		t.Error("the image environment is written BEFORE GitHub's build, which is what " +
			"writes the /etc/environment it is read from")
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
