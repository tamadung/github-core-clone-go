// main.ts — thin launcher for the Go forge.
//
// The harness runs `node /bot/main.ts`. The forge itself is the Go binary
// ./forge (a static Linux amd64 binary, committed as vendored source): it
// binds an ephemeral port, registers it with the env, serves the forge and
// exits 0 when the env says done. This file only starts that binary and
// waits for it, forwarding its stdio, so the Node side never touches the
// wire and the Go side owns the whole lifecycle.
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const dir = path.dirname(fileURLToPath(import.meta.url));
const binary = path.join(dir, 'forge');

const child = spawn(binary, [], { stdio: 'inherit', env: process.env });
child.on('error', (err) => {
  process.stderr.write(`[launcher] cannot start ./forge: ${err.message}\n`);
  process.exit(1);
});
child.on('exit', (code, signal) => {
  if (signal) {
    process.stderr.write(`[launcher] forge stopped by ${signal}\n`);
    process.exit(1);
  }
  process.exit(code ?? 0);
});
