#!/usr/bin/env node
// billet's container hook: GitHub's reference docker hook with billet's mounts.
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
//
// THE GO CACHE HELPER IS THE OTHER MOUNT. A tier with the go cache names billet
// as the go command's GOCACHEPROG, and a job container has neither the binary
// nor the runner's environment, so the hook mounts the one and copies the few
// variables the helper needs. Each is added only where the agent said its cache
// is on: the shim under interception, the helper under the go cache.
'use strict';

const fs = require('fs');
const path = require('path');
const { spawn } = require('child_process');

const UPSTREAM = path.join(__dirname, 'upstream.js');
const SHIM = '/opt/billet/bin/docker';
const BILLET = '/opt/billet/bin/billet';

// The variables the Go cache helper reads, copied into a job container as they
// are in the runner's environment.
const GO_CACHE_ENV = ['GOCACHEPROG', 'GOFLAGS', 'BILLET_CACHE_ENDPOINT', 'BILLET_CACHE_TOKEN'];

function mount(container, file) {
  if (!Array.isArray(container.systemMountVolumes)) {
    container.systemMountVolumes = [];
  }
  container.systemMountVolumes.push({
    sourceVolumePath: file,
    targetVolumePath: file,
    readOnly: true,
  });
}

function addBilletMounts(request, env, paths) {
  if (!request || request.command !== 'prepare_job' || !request.args || !request.args.container) {
    return request;
  }
  const container = request.args.container;
  // Each file may be absent on an image that predates it; a mount of a missing
  // file would make docker create a directory under that name in the container.
  if (env.BILLET_CONTAINER_SHIM === '1' && fs.existsSync(paths.shim)) {
    mount(container, paths.shim);
  }
  if (env.GOCACHEPROG && fs.existsSync(paths.billet)) {
    mount(container, paths.billet);
    const variables = container.environmentVariables || {};
    for (const name of GO_CACHE_ENV) {
      // A job's own env wins, so GOCACHEPROG= still turns the cache off.
      if (env[name] !== undefined && !(name in variables)) {
        variables[name] = env[name];
      }
    }
    container.environmentVariables = variables;
  }
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
    child.stdin.end(JSON.stringify(addBilletMounts(request, process.env, { shim: SHIM, billet: BILLET })));
  });
}

if (require.main === module) {
  main();
}

module.exports = { addBilletMounts };
