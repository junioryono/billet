"use strict";

const fs = require("fs");

for (const name of ["container", "builder", "state-path", "mount-limit-bytes", "discard-marker"]) {
  const value = process.env[`INPUT_${name.toUpperCase()}`];
  if (!value || /[\r\n]/.test(value)) throw new Error(`${name} is unavailable`);
  fs.appendFileSync(process.env.GITHUB_STATE, `${name.replace("-", "_")}=${value}\n`, "utf8");
}
