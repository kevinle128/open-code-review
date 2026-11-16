// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// PM2 app for `ocr serve` on a host. The service settings come from
// ~/ocr-review/ocr-review.env (KEY=VALUE lines, the same variables as the
// Komodo stack). PM2 has no env file option, so this file reads it; edits
// take effect on `pm2 startOrReload ecosystem.config.cjs --update-env`.
//
//   OCR_REVIEW_ROOT      default ~/ocr-review; the working directory.
//   OCR_REVIEW_ENV_FILE  default $OCR_REVIEW_ROOT/ocr-review.env.
//   OCR_REVIEW_BIN       default $OCR_REVIEW_ROOT/bin/ocr.
'use strict';

const fs = require('fs');
const os = require('os');
const path = require('path');

const root = process.env.OCR_REVIEW_ROOT || path.join(os.homedir(), 'ocr-review');
const envFile = process.env.OCR_REVIEW_ENV_FILE || path.join(root, 'ocr-review.env');

// loadEnv parses KEY=VALUE lines. Blank lines and # comments are skipped,
// an `export ` prefix is allowed, and one pair of matching quotes around a
// value is removed. Nothing is unescaped: a GitHub App key stays on one
// line with literal \n, which the service turns back into PEM. Errors
// name the line, never its value.
function loadEnv(file) {
  const env = {};
  const lines = fs.readFileSync(file, 'utf8').split(/\r?\n/);
  lines.forEach((raw, i) => {
    let line = raw.trim();
    if (line === '' || line.startsWith('#')) {
      return;
    }
    if (line.startsWith('export ')) {
      line = line.slice('export '.length).trim();
    }
    const eq = line.indexOf('=');
    const key = eq > 0 ? line.slice(0, eq).trim() : '';
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) {
      throw new Error(`${file}:${i + 1}: expected KEY=VALUE`);
    }
    let value = line.slice(eq + 1).trim();
    const q = value[0];
    if (value.length >= 2 && (q === '"' || q === "'") && value.endsWith(q)) {
      value = value.slice(1, -1);
    }
    env[key] = value;
  });
  return env;
}

module.exports = {
  apps: [
    {
      name: 'ocr-review',
      script: process.env.OCR_REVIEW_BIN || path.join(root, 'bin', 'ocr'),
      args: 'serve',
      // A Go binary, not a Node script.
      interpreter: 'none',
      exec_mode: 'fork',
      instances: 1,
      cwd: root,
      env: loadEnv(envFile),
      autorestart: true,
      restart_delay: 5000,
      max_memory_restart: '1G',
      // The service drains running reviews for up to 60 seconds after
      // SIGTERM. Only the service gets the signal: the docker clients of
      // running reviews must not be stopped before the drain ends.
      kill_signal: 'SIGTERM',
      kill_timeout: 75000,
      treekill: false,
      merge_logs: true,
      time: true,
    },
  ],
};

module.exports.loadEnv = loadEnv;
