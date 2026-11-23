// Hub backup: runs `bd backup sync` against the agent-coordination Dolt server.
// Scheduled via Bun.cron (see register-hub-backup.ts). Non-load-bearing: the hub
// stays usable without it; it only refreshes .beads/backup for recovery.
import { appendFileSync, mkdirSync, rmSync } from "node:fs";
import { homedir } from "node:os";

const HOME = homedir();
const HUB = `${HOME}/Code/agent-coordination`;
const LOG_DIR = `${HOME}/.local/state/dolt-hub`;
const LOG = `${LOG_DIR}/backup.log`;
const LOCK = `${LOG_DIR}/backup.lock`;
const TIMEOUT_MS = 180_000;

const log = (msg: string) => appendFileSync(LOG, `${new Date().toISOString()} ${msg}\n`);

export default {
  async scheduled() {
    mkdirSync(LOG_DIR, { recursive: true });
    try {
      mkdirSync(LOCK); // atomic: fails if a previous run is still going
    } catch {
      log("skip: previous backup still running");
      return;
    }
    try {
      const started = Date.now();
      const proc = Bun.spawn([`${HOME}/.local/bin/bd`, "backup", "sync"], {
        cwd: HUB,
        env: {
          ...process.env,
          BEADS_DIR: `${HUB}/.beads`,
          PATH: `${HOME}/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin`,
        },
        stdout: "pipe",
        stderr: "pipe",
        timeout: TIMEOUT_MS,
      });
      const [out, err, code] = await Promise.all([
        new Response(proc.stdout).text(),
        new Response(proc.stderr).text(),
        proc.exited,
      ]);
      const secs = ((Date.now() - started) / 1000).toFixed(1);
      if (code === 0) log(`ok ${secs}s ${out.trim().split("\n").pop()}`);
      else {
        log(`FAIL exit=${code} ${secs}s ${(err || out).trim().split("\n").pop()}`);
        throw new Error(`bd backup sync failed (exit ${code})`);
      }
    } finally {
      rmSync(LOCK, { recursive: true, force: true });
    }
  },
};
