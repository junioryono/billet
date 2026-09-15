package lifeops

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func realRetirementRoleAlias(t *testing.T, bypass bool) {
	t.Helper()
	h := newRealOperationHost(t)
	server, node := h.prefix+"-server.service", h.prefix+"-node.service"
	for _, unit := range []string{server, node} {
		h.write(unit, "[Unit]\nDefaultDependencies=no\n[Service]\nType=exec\nExecStart=/bin/sleep infinity\n")
	}
	h.run("daemon-reload")
	h.run("start", "--", server, node)
	p := OperationProtection{Units: []string{server, node}}
	sequence := []Operation{{Verb: "stop", Unit: server}}
	if err := h.admit(sequence, p); err != nil {
		t.Fatalf("distinct real roles refused: %v", err)
	}
	h.run("stop", "--", server)
	alias := filepath.Join("/run/systemd/system", server)
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(node, alias); err != nil {
		t.Fatal(err)
	}
	h.run("daemon-reload")
	if h.property(server, "Id") != node || !slices.Contains(strings.Fields(h.property(node, "Names")), server) {
		t.Fatal("runtime symlink did not resolve the controller name to the node")
	}
	original := h.property(node, "InvocationID")
	if original == "" {
		t.Fatal("retained node has no original invocation")
	}
	if !bypass {
		if err := h.admit(sequence, p); err == nil || !strings.Contains(err.Error(), "operation-role-collision") {
			t.Fatalf("real controller/node alias admitted: %v", err)
		}
		if h.property(node, "ActiveState") != "active" || h.property(node, "InvocationID") != original {
			t.Fatal("alias refusal changed the retained node")
		}
		return
	}
	h.run("stop", "--", server)
	if h.property(node, "ActiveState") != "inactive" {
		t.Fatal("counterfactual alias stop did not stop the retained node")
	}
	t.Log("counterfactual controller alias stopped the retained node")
}

func realRetirementPrivateTmp(t *testing.T, role string, bypass bool) {
	t.Helper()
	h := newRealOperationHost(t)
	server, node, backup := h.prefix+"-server.service", h.prefix+"-node.service", h.prefix+"-backup.service"
	owner := h.prefix + "-" + role + ".service"
	finish := filepath.Join("/run", h.prefix, "finish-backup")
	if err := os.MkdirAll(filepath.Dir(finish), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, unit := range []string{server, node, backup} {
		body := "[Unit]\nDefaultDependencies=no\n[Service]\nType=exec\nExecStart=/bin/sleep infinity\n"
		if unit == backup {
			body = "[Unit]\nDefaultDependencies=no\n[Service]\nType=oneshot\nExecStart=/bin/sh -c 'while test ! -e " + finish + "; do sleep 0.02; done'\n"
		}
		if unit == owner {
			body += "PrivateTmp=yes\n"
		} else {
			body += "PrivateTmp=no\n"
		}
		h.write(unit, body)
	}
	h.run("daemon-reload")
	h.run("start", "--", server, node)
	if role == "backup" {
		h.run("start", "--no-block", "--", backup)
	}
	// Observe the manager-created directory independently of admission's reader.
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	prefix := "systemd-private-" + strings.ReplaceAll(strings.TrimSpace(string(boot)), "-", "") + "-" + owner + "-"
	tree := ""
	deadline := time.Now().Add(5 * time.Second)
	for tree == "" {
		entries, err := os.ReadDir("/var/tmp")
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) {
				candidate := filepath.Join("/var/tmp", entry.Name(), "tmp")
				info, err := os.Stat(candidate)
				if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				if err == nil && info.IsDir() {
					tree = candidate
				}
			}
		}
		if tree == "" {
			if time.Now().After(deadline) {
				t.Fatal("systemd did not create the owner's private /var/tmp tree")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if role == "backup" && (h.property(backup, "ActiveState") != "activating" || h.property(backup, "SubState") != "start" || h.property(backup, "Job") == "") {
		t.Fatal("backup witness is not an in-flight oneshot with a start job")
	}
	p := OperationProtection{Units: []string{server, node, backup}, QuietUnits: []string{server, backup}, WaitingUnits: []string{backup}}
	sequence := []Operation{{Verb: "stop", Unit: server}}
	if err := h.admit(sequence, p); err != nil {
		t.Fatalf("real private-tmp control refused: %v", err)
	}
	key := filepath.Join(tree, "node.key")
	if err := os.WriteFile(key, []byte("retained node TLS key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.UnitPaths = map[string][]string{node: {key}}
	original := h.property(node, "InvocationID")
	if !bypass {
		if err := h.admit(sequence, p); err == nil || !strings.Contains(err.Error(), "operation-directory-overlap: "+owner+" PrivateTmp=") {
			t.Fatalf("real private-tmp teardown admitted: %v", err)
		}
		body, err := os.ReadFile(key)
		if err != nil || string(body) != "retained node TLS key\n" || h.property(server, "ActiveState") != "active" ||
			h.property(node, "ActiveState") != "active" || h.property(node, "InvocationID") != original {
			t.Fatalf("private-tmp refusal changed controller, node or retained key: %v", err)
		}
		return
	}
	h.run("stop", "--", server)
	if role == "backup" {
		// Let the oneshot finish normally; retirement never stops this service.
		if err := os.WriteFile(finish, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		_, err := os.Lstat(key)
		if os.IsNotExist(err) && h.property(owner, "ActiveState") == "inactive" {
			if h.property(node, "ActiveState") != "active" || h.property(node, "InvocationID") != original {
				t.Fatal("counterfactual changed the node process instead of only deleting its key")
			}
			t.Logf("counterfactual %s teardown removed the retained key from private /var/tmp", role)
			return
		}
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("counterfactual teardown did not remove the retained private-tmp key")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
