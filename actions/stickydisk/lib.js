"use strict";

const fs = require("fs");
const http = require("http");
const https = require("https");
const { spawnSync } = require("child_process");

const MMDS = "http://169.254.169.254";

function warning(message) {
  const safe = String(message).replace(/%/g, "%25").replace(/\r/g, "%0D").replace(/\n/g, "%0A");
  process.stdout.write(`::warning::${safe}\n`);
}

function output(name, value) {
  const target = process.env.GITHUB_OUTPUT;
  if (target) fs.appendFileSync(target, `${name}=${value}\n`, { encoding: "utf8" });
}

function saveState(name, value) {
  const target = process.env.GITHUB_STATE;
  if (!target) throw new Error("GITHUB_STATE is unavailable");
  if (String(value).includes("\n") || String(value).includes("\r")) throw new Error(`${name} contains a line break`);
  fs.appendFileSync(target, `${name}=${value}\n`, { encoding: "utf8" });
}

function run(command, args, options = {}) {
  const result = spawnSync(command, args, {
    encoding: "utf8",
    timeout: options.timeout || 30000,
    maxBuffer: 1024 * 1024,
    stdio: options.capture === false ? "inherit" : ["ignore", "pipe", "pipe"],
  });
  if (result.error) throw result.error;
  if (result.status !== 0 && !options.allowFailure) {
    const detail = String(result.stderr || result.stdout || "").trim();
    throw new Error(`${command} exited ${result.status}${detail ? `: ${detail}` : ""}`);
  }
  return result;
}

function request(url, options = {}, body) {
  return new Promise((resolve, reject) => {
    const parsed = new URL(url);
    if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
      reject(new Error("cache metadata named a non-HTTP(S) endpoint"));
      return;
    }
    const transport = parsed.protocol === "https:" ? https : http;
    const req = transport.request(parsed, {
      method: options.method || "GET",
      headers: options.headers || {},
      timeout: options.timeout || 10000,
    }, (res) => {
      let response = "";
      res.setEncoding("utf8");
      res.on("data", (chunk) => {
        response += chunk;
        if (response.length > 65536) req.destroy(new Error("cache response is too large"));
      });
      res.on("end", () => {
        if (res.statusCode < 200 || res.statusCode >= 300) {
          reject(new Error(`cache service returned HTTP ${res.statusCode}`));
          return;
        }
        resolve(response);
      });
    });
    req.on("timeout", () => req.destroy(new Error("cache request timed out")));
    req.on("error", reject);
    if (body !== undefined) req.write(body);
    req.end();
  });
}

async function credentials() {
  if (process.env.BILLET_CACHE_ENDPOINT && process.env.BILLET_CACHE_TOKEN) {
    return { endpoint: process.env.BILLET_CACHE_ENDPOINT, token: process.env.BILLET_CACHE_TOKEN };
  }
  const token = await request(`${MMDS}/latest/api/token`, {
    method: "PUT",
    headers: { "X-metadata-token-ttl-seconds": "60" },
  });
  const headers = { "X-metadata-token": token };
  const endpoint = await request(`${MMDS}/latest/meta-data/billet/cache-endpoint`, { headers });
  const cacheToken = await request(`${MMDS}/latest/meta-data/billet/cache-token`, { headers });
  return { endpoint: endpoint.trim(), token: cacheToken.trim() };
}

async function cacheCall(endpoint, token, path, body, timeout = 120000) {
  const url = new URL(path, endpoint.endsWith("/") ? endpoint : `${endpoint}/`);
  const encoded = body === undefined ? undefined : JSON.stringify(body);
  const response = await request(url, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${token}`,
      ...(encoded === undefined ? {} : { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(encoded) }),
    },
    timeout,
  }, encoded);
  return JSON.parse(response);
}

module.exports = { cacheCall, credentials, output, run, saveState, warning };
