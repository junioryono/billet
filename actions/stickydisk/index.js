"use strict";

const fs = require("fs");
const path = require("path");
const { cacheCall, credentials, output, run, saveState, warning } = require("./lib");

function mountPath(input) {
  if (!input || /[\0\r\n]/.test(input)) throw new Error("path is empty or contains a control character");
  if (input === "~") return process.env.HOME;
  if (input.startsWith("~/")) return path.join(process.env.HOME, input.slice(2));
  return path.resolve(process.env.GITHUB_WORKSPACE || process.cwd(), input);
}

async function discard(endpoint, token, slot) {
  try {
    await cacheCall(endpoint, token, `/v1/volumes/${slot}/discard`);
  } catch (error) {
    warning(`billet could not discard an unusable sticky disk; node cleanup will retry: ${error.message}`);
  }
}

async function main() {
  output("cache-hit", "false");
  let endpoint;
  let token;
  let slot;
  let target;
  let mounted = false;
  try {
    const key = process.env.INPUT_KEY || "";
    if (!key.trim() || key.length > 512 || /[\0\r\n]/.test(key)) throw new Error("key is empty, too long, or contains a control character");
    target = mountPath(process.env.INPUT_PATH || "");
    const sizeGB = Number(process.env["INPUT_SIZE-GB"] || "10");
    if (!Number.isSafeInteger(sizeGB) || sizeGB < 1 || sizeGB > 100) throw new Error("size-gb must be a whole number from 1 through 100");
    const discardMarker = process.env["INPUT_DISCARD-MARKER"] ? mountPath(process.env["INPUT_DISCARD-MARKER"]) : "";
    if (discardMarker) {
      fs.writeFileSync(discardMarker, "", { mode: 0o600 });
      saveState("discard_marker", discardMarker);
    }

    ({ endpoint, token } = await credentials());
    const attached = await cacheCall(endpoint, token, "/v1/volumes", {
      key,
      size_bytes: sizeGB * 1024 * 1024 * 1024,
      publication: process.env.INPUT_PUBLICATION || "cas",
    });
    slot = attached.slot;

    for (let attempt = 0; attempt < 100; attempt += 1) {
      if (run("test", ["-b", attached.device], { allowFailure: true }).status === 0) break;
      await new Promise((resolve) => setTimeout(resolve, 100));
      if (attempt === 99) throw new Error(`${attached.device} did not appear in the guest`);
    }

    const type = run("sudo", ["blkid", "-o", "value", "-s", "TYPE", attached.device], { allowFailure: true });
    const filesystem = String(type.stdout || "").trim();
    if (!filesystem) run("sudo", ["mkfs.ext4", "-F", attached.device], { capture: false, timeout: 120000 });
    else if (filesystem !== "ext4") throw new Error(`sticky disk contains ${filesystem}, not ext4`);

    run("sudo", ["mkdir", "-p", target]);
    run("sudo", ["mount", "-t", "ext4", "-o", "noatime", attached.device, target]);
    mounted = true;
    run("sudo", ["chown", `${process.getuid()}:${process.getgid()}`, target]);

    saveState("endpoint", endpoint);
    saveState("token", token);
    saveState("slot", String(slot));
    saveState("device", attached.device);
    saveState("path", target);
    saveState("mounted", "true");
    output("cache-hit", attached.cold ? "false" : "true");
    process.stdout.write(`billet sticky disk mounted at ${target}\n`);
  } catch (error) {
    warning(`billet sticky disk is unavailable; this job will continue cold: ${error.message}`);
    if (mounted) {
      const result = run("sudo", ["umount", target], { allowFailure: true, timeout: 120000 });
      if (result.status !== 0) {
        warning("billet left the unusable disk attached because it could not be safely unmounted; node cleanup will discard it after the guest stops");
        return;
      }
    }
    if (endpoint && token && Number.isInteger(slot)) await discard(endpoint, token, slot);
  }
}

main().catch((error) => warning(`billet sticky disk is unavailable; this job will continue cold: ${error.message}`));
