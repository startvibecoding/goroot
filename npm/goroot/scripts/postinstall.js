#!/usr/bin/env node
'use strict';

// Download the platform binary from the matching GitHub Release into vendor/.
// The release asset name is goroot_<version>_<os>_<arch> (a raw binary).

const fs = require('fs');
const http = require('http');
const https = require('https');
const path = require('path');

const pkg = require('../package.json');
const version = pkg.version;
const REPO = process.env.GOROOT_REPO || 'startvibecoding/goroot';

function skip(msg) {
  console.log(`goroot: ${msg}`);
  process.exit(0); // never hard-fail the npm install
}

if (process.env.GOROOT_SKIP_DOWNLOAD) {
  skip('GOROOT_SKIP_DOWNLOAD set, skipping binary download');
}
if (process.platform !== 'linux') {
  skip(`unsupported platform ${process.platform} (goroot is Linux-only)`);
}

const ARCH = { x64: 'amd64', arm64: 'arm64' }[process.arch];
if (!ARCH) {
  skip(`unsupported architecture ${process.arch}`);
}

const base = process.env.GOROOT_DOWNLOAD_BASE ||
  `https://github.com/${REPO}/releases/download/v${version}`;
const asset = `goroot_${version}_linux_${ARCH}`;
const destDir = path.join(__dirname, '..', 'vendor');
const dest = path.join(destDir, 'goroot');

function download(url, cb) {
  const client = url.startsWith('http://') ? http : https;
  client.get(url, { headers: { 'User-Agent': 'goroot-npm' } }, (res) => {
    if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
      res.resume();
      return download(res.headers.location, cb);
    }
    if (res.statusCode !== 200) {
      res.resume();
      return cb(new Error(`HTTP ${res.statusCode} for ${url}`));
    }
    cb(null, res);
  }).on('error', cb);
}

fs.mkdirSync(destDir, { recursive: true });

download(`${base}/${asset}`, (err, res) => {
  if (err) {
    skip(`could not download ${base}/${asset} (${err.message}); ` +
      'install manually from the releases page');
  }
  const tmp = `${dest}.tmp`;
  const out = fs.createWriteStream(tmp);
  res.pipe(out);
  out.on('error', (e) => skip(`write failed: ${e.message}`));
  out.on('finish', () => {
    fs.renameSync(tmp, dest);
    fs.chmodSync(dest, 0o755);
    console.log(`goroot ${version} (${ARCH}) installed`);
  });
});
