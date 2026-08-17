"use strict";

const { spawnSync } = require("child_process");
const fs = require("fs");
const path = require("path");

const usageFile = ".billet-buildkit-cachemounts.json";

function run(command, args) {
  return spawnSync(command, args, { encoding: "utf8", timeout: 120000, maxBuffer: 4 * 1024 * 1024 });
}

const container = process.env.STATE_container;
const builder = process.env.STATE_builder;
const statePath = process.env.STATE_state_path;
const discardMarker = process.env.STATE_discard_marker;
const mountLimit = Number(process.env.STATE_mount_limit_bytes || 20 * 1024 * 1024 * 1024);

function successful(result, operation) {
  if (result.error) throw result.error;
  if (result.status !== 0) {
    const detail = String(result.stderr || result.stdout || "").trim();
    throw new Error(`${operation} exited ${result.status}${detail ? `: ${detail}` : ""}`);
  }
  return result;
}

function cacheMountUsage() {
  const result = successful(run("docker", [
    "exec", container, "buildctl", "--addr", "unix:///run/buildkit/buildkitd.sock",
    "du", "--filter", "type==exec.cachemount", "--format", "{{json .}}",
  ]), "buildctl du");
  const records = JSON.parse(result.stdout || "[]");
  if (!Array.isArray(records)) throw new Error("buildctl du did not return an array");
  for (const record of records) {
    if (!record || typeof record.id !== "string" || !record.id ||
        typeof record.description !== "string" ||
        !Number.isSafeInteger(record.size) || record.size < 0 ||
        record.recordType !== "exec.cachemount") {
      throw new Error("buildctl du returned an invalid cache-mount record");
    }
  }
  return records;
}

function previousUsage() {
  try {
    const parsed = JSON.parse(fs.readFileSync(path.join(statePath, usageFile), "utf8"));
    return parsed && typeof parsed === "object" && !Array.isArray(parsed) ? parsed : {};
  } catch (error) {
    if (error.code !== "ENOENT") process.stdout.write(`::warning::billet could not read prior BuildKit mount usage: ${error.message}\n`);
    return {};
  }
}

function formatBytes(value) {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let amount = Math.abs(value);
  let unit = 0;
  while (amount >= 1024 && unit < units.length - 1) {
    amount /= 1024;
    unit += 1;
  }
  const rendered = Number.isInteger(amount) ? String(amount) : amount.toFixed(1);
  return `${value < 0 ? "-" : ""}${rendered} ${units[unit]}`;
}

function reportUsage(records, prior) {
  for (const record of records) {
    const before = Number(prior[record.description]?.size || 0);
    const growth = record.size - before;
    process.stdout.write(`BuildKit cache mount ${record.description}: ${formatBytes(record.size)} (${growth >= 0 ? "grew " : "shrunk "}${formatBytes(Math.abs(growth))})\n`);
  }
}

function recordUsage(records) {
  const next = {};
  for (const record of records) {
    next[record.description] = { size: record.size };
  }
  fs.writeFileSync(path.join(statePath, usageFile), JSON.stringify(next), { encoding: "utf8", mode: 0o600 });
}

function enforceMountLimit() {
  if (!Number.isSafeInteger(mountLimit) || mountLimit <= 0) {
    throw new Error("the tier supplied an invalid BuildKit cache-mount ceiling");
  }
  const prior = previousUsage();
  const records = cacheMountUsage();
  reportUsage(records, prior);
  for (const record of records) {
    if (record.size <= mountLimit) continue;
    process.stdout.write(`BuildKit cache mount ${record.description} exceeds ${formatBytes(mountLimit)}; resetting that mount\n`);
    successful(run("docker", [
      "exec", container, "buildctl", "--addr", "unix:///run/buildkit/buildkitd.sock",
      "prune", "--filter", `id==${record.id}`,
    ]), `prune ${record.id}`);
  }
  const after = cacheMountUsage();
  if (after.some((record) => record.size > mountLimit)) {
    throw new Error("an oversized BuildKit cache mount remained after pruning");
  }
  recordUsage(after);
  if (!discardMarker) throw new Error("the runner-local publication guard is unavailable");
  fs.rmSync(discardMarker);
}

function markDiscard(error) {
  const message = String(error.message || error).replace(/%/g, "%25").replace(/\r/g, "%0D").replace(/\n/g, "%0A");
  process.stdout.write(`::warning::billet could not enforce the BuildKit cache-mount ceiling and will discard this cache update: ${message}\n`);
}

if (container) {
	try {
		enforceMountLimit();
	} catch (error) {
		markDiscard(error);
	}
	run("docker", ["rm", "--force", container]);
}
if (builder) run("docker", ["buildx", "rm", builder]);
if (statePath) {
  const size = run("du", ["-sh", statePath]);
  if (size.status === 0 && size.stdout) process.stdout.write(`Persistent BuildKit state: ${size.stdout}`);
}
