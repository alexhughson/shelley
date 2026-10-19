// Playwright global setup: starts a shelley test server on a random port.
// The actual port is communicated via --port-file, then exported as
// PLAYWRIGHT_TEST_BASE_URL so every worker's baseURL fixture picks it up.

import { execFileSync, execSync, spawn, type ChildProcess } from 'child_process';
import { mkdirSync, existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'fs';
import { createServer, type Server } from 'http';
import type { AddressInfo } from 'net';
import { tmpdir } from 'os';
import path from 'path';
import { fileURLToPath } from 'url';
import { hermeticGitEnvironment, installHermeticGitEnvironment } from './git-test-env';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const shelleyDir = path.resolve(__dirname, '../..');
const binPath = path.join(shelleyDir, 'bin', 'shelley');

let serverProcess: ChildProcess | null = null;
let tempDir: string | null = null;
let liveProvider: Server | null = null;

// GPT-Live's session endpoint, as far as Shelley sees it: each call gets a
// new session id. Media and events never reach it; the specs fake the
// browser's WebRTC side.
async function startLiveProvider(): Promise<string> {
  let sessions = 0;
  liveProvider = createServer((req, res) => {
    req.resume();
    res.writeHead(201, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ session: { id: `live_${++sessions}` }, transport: { type: 'webrtc', sdp: 'v=0' } }));
  });
  await new Promise<void>((resolve) => liveProvider!.listen(0, '127.0.0.1', resolve));
  return `http://127.0.0.1:${(liveProvider.address() as AddressInfo).port}/v1/live/sessions`;
}

// Terminal sessions outlive the server by design, so end the ones the tests
// left behind, along with every process group in each owned PTY session.
// Interactive shells put foreground and background jobs in separate groups;
// killing only the forkpty child's group leaves those jobs running forever.
function killTerminalSessions(dir: string) {
  const prefix = `exe-scroll: session ${dir}/`;
  // Shared CI hosts can have process tables larger than Node's 1 MiB default.
  const procs = execFileSync('ps', ['-axo', 'pid=,ppid=,pgid=,sess=,command='], {
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024,
  })
    .split('\n')
    .flatMap((line) => {
      const m = /^\s*(\d+)\s+(\d+)\s+(\d+)\s+(\S+)\s+(.*)$/.exec(line);
      return m ? [{
        pid: Number(m[1]), ppid: Number(m[2]), pgid: Number(m[3]),
        session: m[4], command: m[5],
      }] : [];
    });
  const servers = procs.filter((p) => p.command.startsWith(prefix));
  const serverSessions = new Map(servers.map((p) => [p.pid, p.session]));
  // Linux ps prints the numeric SID; macOS ps prints a hex session pointer.
  // forkpty's direct child leads its own group in a *different* session
  // from the server. Match that session token exactly for every group.
  const sessions = new Set(procs.filter((p) => p.pid === p.pgid && serverSessions.has(p.ppid)
    && p.session !== serverSessions.get(p.ppid)).map((p) => p.session));
  const kill = (pid: number) => {
    try {
      process.kill(pid, 'SIGKILL');
    } catch {
      // already gone
    }
  };
  const groups = new Set(procs.filter((p) => sessions.has(p.session)).map((p) => p.pgid));
  for (const group of groups) kill(-group);
  for (const server of servers) kill(server.pid);
}

export default async function globalSetup() {
  const originalEnvironment = { ...process.env };
  // Keep fixture commands, Playwright workers, and the managed Shelley server
  // independent of the developer's global/system git config and hooks.
  const restoreGitEnvironment = installHermeticGitEnvironment();

  const cleanup = () => {
    const dir = tempDir;
    tempDir = null;
    try {
      if (dir) {
        killTerminalSessions(dir);
        rmSync(dir, { recursive: true, force: true });
      }
    } finally {
      liveProvider?.close();
      liveProvider = null;
      delete process.env.SHELLEY_TEST_CWD;
      restoreGitEnvironment();
    }
  };

  try {
    return await startTestEnvironment(cleanup, originalEnvironment);
  } catch (error) {
    if (serverProcess) {
      serverProcess.kill('SIGKILL');
      serverProcess = null;
    }
    cleanup();
    throw error;
  }
}

async function startTestEnvironment(cleanup: () => void, originalEnvironment: NodeJS.ProcessEnv) {
  // Give every shard its own home and cwd. The tests edit user AGENTS.md,
  // and prompt hydration scans cwd for guidance/skills: sharing the runner's
  // HOME or walking all of /tmp makes unrelated builds interfere.
  tempDir = mkdtempSync(path.join(tmpdir(), 'shelley-e2e-'));
  const cwd = path.join(tempDir, 'cwd');
  const home = path.join(tempDir, 'home');
  mkdirSync(cwd);
  mkdirSync(home);
  process.env.SHELLEY_TEST_CWD = cwd;

  // External servers keep their own environment; API fixtures still get a
  // small, real directory instead of the shared /tmp tree.
  if (process.env.TEST_SERVER_URL) {
    process.env.PLAYWRIGHT_TEST_BASE_URL = process.env.TEST_SERVER_URL;
    return cleanup;
  }

  // Git-viewer specs need real history, not the runner's giant checkout.
  const git = (...args: string[]) => execFileSync('git', [
    '-C', cwd, '-c', 'user.name=Shelley Test', '-c', 'user.email=test@example.com',
    '-c', 'commit.gpgsign=false', '-c', 'core.hooksPath=/dev/null', ...args,
  ], { env: hermeticGitEnvironment({ ...process.env, HOME: home }) });
  git('init', '--quiet', '--initial-branch=main');
  writeFileSync(path.join(cwd, 'example.txt'), 'before\n');
  git('add', 'example.txt');
  git('commit', '--quiet', '-m', 'Initial test fixture');
  writeFileSync(path.join(cwd, 'example.txt'), 'after\n');
  git('commit', '--quiet', '-am', 'Update test fixture');

  // Building is not part of Git fixture isolation and may need the developer's
  // Git config for module fetching or version stamping.
  if (!existsSync(binPath)) {
    console.log('Building shelley binary…');
    execSync('go build -o bin/shelley ./cmd/shelley', {
      cwd: shelleyDir,
      stdio: 'inherit',
      env: originalEnvironment,
    });
  }

  // Database and port file stay outside the conversation working directory.
  const testDb = path.join(tempDir, 'test.db');
  const portFile = path.join(tempDir, 'port');
  const socketPath = path.join(tempDir, 'client.sock');
  process.env.TEST_SERVER_SOCKET = socketPath;

  const liveSessionURL = await startLiveProvider();
  console.log(`Starting shelley (db=${testDb}, port-file=${portFile})`);

  let earlyExit = false;
  let exitCode: number | null = null;

  serverProcess = spawn(binPath, [
    '--predictable-only',
    '--db', testDb,
    'serve',
    '--port', '0',
    '--port-file', portFile,
    '--socket', socketPath,
  ], {
    cwd,
    stdio: 'inherit',
    env: hermeticGitEnvironment({
      ...process.env,
      HOME: home,
      PWD: cwd,
      PREDICTABLE_DELAY_MS: process.env.PREDICTABLE_DELAY_MS || '20',
      SHELLEY_LIVE_SESSION_URL: liveSessionURL,
    }),
  });

  serverProcess.on('exit', (code) => {
    earlyExit = true;
    exitCode = code;
  });

  // Wait for port file to appear (server has bound).
  const deadline = Date.now() + 30_000;
  while (!existsSync(portFile)) {
    if (earlyExit) {
      throw new Error(`Shelley server exited (code ${exitCode}) before writing port file`);
    }
    if (Date.now() > deadline) {
      throw new Error('Shelley server did not write port file within 30s');
    }
    await new Promise(r => setTimeout(r, 50));
  }

  const port = readFileSync(portFile, 'utf8').trim();
  const baseURL = `http://localhost:${port}`;
  console.log(`Shelley test server listening at ${baseURL}`);

  // Wait for the server to actually respond to HTTP.
  const httpDeadline = Date.now() + 30_000;
  let httpReady = false;
  while (Date.now() < httpDeadline) {
    if (earlyExit) {
      throw new Error(`Shelley server exited (code ${exitCode}) during startup`);
    }
    try {
      const res = await fetch(baseURL);
      if (res.ok) { httpReady = true; break; }
    } catch {
      // not ready yet
    }
    await new Promise(r => setTimeout(r, 100));
  }
  if (!httpReady) {
    throw new Error(`Shelley server at ${baseURL} never responded OK within 30s`);
  }

  // Playwright's built-in baseURL fixture reads this env var.
  process.env.PLAYWRIGHT_TEST_BASE_URL = baseURL;

  // Reap the server before removing files it may still be writing.
  return async () => {
    const server = serverProcess!;
    try {
      if (server.exitCode !== null || server.signalCode !== null) {
        throw new Error(`Shelley exited before teardown (code ${server.exitCode}, signal ${server.signalCode})`);
      }
      const exited = new Promise<void>((resolve) => server.once('exit', () => resolve()));
      let timedOut = false;
      const deadline = setTimeout(() => {
        timedOut = true;
        server.kill('SIGKILL');
      }, 10_000);
      try {
        server.kill('SIGTERM');
        await exited;
        if (timedOut) throw new Error('Shelley did not exit within 10s of SIGTERM');
      } finally {
        clearTimeout(deadline);
      }
    } finally {
      serverProcess = null;
      cleanup();
    }
  };
}
