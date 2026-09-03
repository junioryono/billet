#!/usr/bin/env node
// billet's container hook: GitHub's reference docker hook with one bind mount.
//
// WHY A HOOK AT ALL. A job that runs in `container:` executes its steps with the
// image's own `docker` client, and the guest's docker shim -- which points a
// build's BuildKit cache client at billet's adapter -- is not on that PATH. The
// runner's container hooks are the one seam that owns how job containers are
// created, so this hook adds the shim to the container's system mounts and
// hands EVERYTHING ELSE to the reference implementation, unchanged: networks,
// volumes, `options:`, services, container actions, script steps. The value of
// this file is how little it does.
//
// THE MOUNT IS UNDER /opt/billet/bin, NOT OVER /usr/local/bin/docker. An image
// that carries its own client there (`docker:cli` does) would have it shadowed
// by a shim with nothing behind it. The job hook prepends /opt/billet/bin to
// GITHUB_PATH instead, so the shim is first on every step's PATH and the
// image's client, wherever it is, is what the shim finds behind itself.
'use strict';

const fs = require('fs');
const path = require('path');
const { spawn } = require('child_process');

const UPSTREAM = path.join(__dirname, 'upstream.js');
const SHIM = '/opt/billet/bin/docker';

function addShimMount(request, shim) {
  if (!request || request.command !== 'prepare_job' || !request.args || !request.args.container) {
    return request;
  }
  // The shim may be absent on an image that predates it; a mount of a missing
  // file would make docker create a directory under that name in the container.
  if (!fs.existsSync(shim)) {
    return request;
  }
  const container = request.args.container;
  if (!Array.isArray(container.systemMountVolumes)) {
    container.systemMountVolumes = [];
  }
  container.systemMountVolumes.push({
    sourceVolumePath: shim,
    targetVolumePath: shim,
    readOnly: true,
  });
  return request;
}

function main() {
  const chunks = [];
  process.stdin.on('data', (chunk) => chunks.push(chunk));
  process.stdin.on('end', () => {
    let request;
    try {
      request = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    } catch (err) {
      process.stderr.write(`billet container hook: unreadable request: ${err.message}\n`);
      process.exit(1);
    }
    const child = spawn(process.execPath, [UPSTREAM], {
      stdio: ['pipe', 'inherit', 'inherit'],
      env: process.env,
    });
    child.on('error', (err) => {
      process.stderr.write(`billet container hook: cannot run the reference hook: ${err.message}\n`);
      process.exit(1);
    });
    child.on('exit', (code, signal) => {
      process.exit(signal ? 1 : code);
    });
    child.stdin.end(JSON.stringify(addShimMount(request, SHIM)));
  });
}

if (require.main === module) {
  main();
}

module.exports = { addShimMount };
