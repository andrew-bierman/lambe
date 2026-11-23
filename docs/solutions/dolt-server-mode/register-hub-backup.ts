// One-time (idempotent) registration of the hub backup as an OS-level cron job.
// Remove with: bun -e 'await Bun.cron.remove("hub-backup")'
await Bun.cron(`${import.meta.dir}/hub-backup.ts`, "*/15 * * * *", "hub-backup");
console.log("registered hub-backup every 15 minutes");
