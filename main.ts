// Launcher for the Go forge. The trial runs `node /bot/main.ts`; this file
// only starts the compiled binary sitting beside it and waits for it to
// finish, passing its exit code through.
//
// The binary itself owns the whole trial protocol: it binds a free port,
// registers that port with the env at $MAKEFUN_ENV_URL, serves git smart
// HTTP, REST v3, GraphQL and MCP from its own store, polls the env until
// the suite is done, then exits 0. With no env configured it instead
// prints its URL and serves until it is stopped, which is how the forge
// is exercised on a developer machine.

import { spawn } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const binary = join(here, 'forge-go');

const child = spawn(binary, [], { stdio: 'inherit', env: process.env });

child.on('error', (cause: Error) => {
  process.stderr.write(`[entry] cannot start forge-go: ${cause.message}\n`);
  process.exit(1);
});

child.on('exit', (code: number | null, signal: NodeJS.Signals | null) => {
  if (signal !== null) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});
