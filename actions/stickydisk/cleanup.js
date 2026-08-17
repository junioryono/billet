"use strict";

const fs = require("fs");
const { cacheCall, run, warning } = require("./lib");

async function cleanup() {
  if (process.env.STATE_mounted !== "true") return;

  const target = process.env.STATE_path;
  const endpoint = process.env.STATE_endpoint;
  const token = process.env.STATE_token;
  const slot = process.env.STATE_slot;
  const discard = Boolean(process.env.STATE_discard_marker) && fs.existsSync(process.env.STATE_discard_marker);
  try {
    run("sudo", ["sync", "-f", target], { allowFailure: true, timeout: 120000 });
    run("sudo", ["umount", target], { timeout: 120000 });
  } catch (error) {
    warning(`billet could not unmount the sticky disk, so it will discard this job's writes: ${error.message}`);
    return;
  }

  try {
    const operation = discard ? "discard" : "commit";
    let body;
    if (!discard) {
      const device = process.env.STATE_device;
      const type = String(run("sudo", ["blkid", "-o", "value", "-s", "TYPE", device]).stdout || "").trim();
      const uuid = String(run("sudo", ["blkid", "-o", "value", "-s", "UUID", device]).stdout || "").trim();
      const checked = run("sudo", ["e2fsck", "-f", "-n", device], { allowFailure: true, timeout: 120000 });
      body = { filesystem: { type, uuid, clean: checked.status === 0 } };
    }
    const result = await cacheCall(endpoint, token, `/v1/volumes/${slot}/${operation}`, body);
    if (discard) {
      warning("billet discarded this sticky-disk update because its cache policy could not be enforced; the job result is unchanged");
      return;
    }
    if (!result.published) warning(`billet discarded this sticky-disk update (${result.reason || "cache unavailable"}); the job result is unchanged`);
  } catch (error) {
    warning(`billet could not commit the sticky disk; the job result is unchanged: ${error.message}`);
  }
}

cleanup().catch((error) => warning(`billet could not commit the sticky disk; the job result is unchanged: ${error.message}`));
