package main

import "fmt"

// kernelToRecord decides what a successful verification should record as the
// generation's kernel pairing, and what to tell the operator.
//
// AN EMPTY FIRST RETURN MEANS RECORD NOTHING, which is not the same as failure:
// the two cases below that produce it are "already correct" and "cannot be
// expressed", and neither is served by writing something.
//
// THE RULE IS THAT VERIFICATION RECORDS WHAT ACTUALLY BOOTED. The launch path
// resolves a generation's recorded kernel in preference to the node's
// configuration, so on any normally-pulled generation the thing that just booted
// IS the thing already recorded -- and writing the configured kernel over it would
// prove one kernel and record another, then publish that through @verified. An
// earlier version of this did exactly that.
func kernelToRecord(existing, configured string) (string, string) {
	if existing != "" {
		return "", fmt.Sprintf("this generation was already paired with %s, which is the "+
			"kernel the launch used and therefore the one this boot proved", existing)
	}

	if configured == "" {
		return "", "this node's kernel is outside the managed directory, so the generation " +
			"stays unpaired and every node will boot it with whatever it is configured " +
			"with. `billet images pull` installs a kernel there and records the pairing"
	}

	return configured, ""
}
