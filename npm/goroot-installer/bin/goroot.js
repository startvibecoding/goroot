#!/usr/bin/env node
'use strict';

// Thin launcher: exec the platform binary downloaded by scripts/postinstall.js.

const { spawnSync } = require('child_process');
const fs = require('fs');
const path = require('path');

const exe = path.join(__dirname, '..', 'vendor', 'goroot');

if (!fs.existsSync(exe)) {
  console.error(`goroot: binary not found at ${exe}`);
  console.error('goroot: the postinstall step may have been skipped or failed.');
  console.error('goroot: reinstall with   npm i -g goroot-installer');
  console.error('goroot: or download a release from https://github.com/startvibecoding/goroot/releases');
  process.exit(1);
}

const res = spawnSync(exe, process.argv.slice(2), { stdio: 'inherit' });
if (res.error) {
  console.error(`goroot: ${res.error.message}`);
  process.exit(1);
}
process.exit(res.status === null ? 1 : res.status);
