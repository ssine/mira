import fs from "node:fs/promises";
import process from "node:process";
import { Pool } from "pg";

import { hashPassword } from "./auth.mjs";
import { initializeDatabase } from "./db.mjs";

async function readHidden(prompt) {
  if (!process.stdin.isTTY || !process.stdin.setRawMode) {
    const chunks = [];
    for await (const chunk of process.stdin) chunks.push(chunk);
    return Buffer.concat(chunks).toString("utf8").replace(/[\r\n]+$/, "");
  }
  process.stderr.write(prompt);
  process.stdin.setRawMode(true);
  process.stdin.resume();
  process.stdin.setEncoding("utf8");
  return new Promise((resolve, reject) => {
    let value = "";
    function finish(error = null) {
      process.stdin.setRawMode(false);
      process.stdin.pause();
      process.stdin.off("data", onData);
      process.stderr.write("\n");
      if (error) reject(error);
      else resolve(value);
    }
    function onData(chunk) {
      for (const character of chunk) {
        if (character === "\u0003") return finish(new Error("cancelled"));
        if (character === "\r" || character === "\n") return finish();
        if (character === "\u007f" || character === "\b") {
          value = value.slice(0, -1);
        } else {
          value += character;
        }
      }
    }
    process.stdin.on("data", onData);
  });
}

async function passwordInput() {
  const passwordFile = process.env.MIRA_ADMIN_PASSWORD_FILE;
  if (passwordFile) {
    return (await fs.readFile(passwordFile, "utf8")).replace(/[\r\n]+$/, "");
  }
  if (!process.stdin.isTTY) return readHidden("");
  const first = await readHidden("New Mira administrator password: ");
  const second = await readHidden("Confirm password: ");
  if (first !== second) throw new Error("passwords do not match");
  return first;
}

async function setPassword(pool, username) {
  if (!/^[a-zA-Z0-9._@-]{1,128}$/.test(username)) throw new Error("invalid administrator username");
  const password = await passwordInput();
  const passwordHash = await hashPassword(password);
  const client = await pool.connect();
  try {
    await client.query("BEGIN");
    const existing = await client.query("SELECT admin_user_id FROM mira_admin_users LIMIT 1");
    let adminUserId;
    if (existing.rowCount === 0) {
      const inserted = await client.query(
        `INSERT INTO mira_admin_users (username, password_hash)
         VALUES ($1, $2) RETURNING admin_user_id`,
        [username, passwordHash],
      );
      adminUserId = inserted.rows[0].admin_user_id;
    } else {
      adminUserId = existing.rows[0].admin_user_id;
      await client.query(
        `UPDATE mira_admin_users SET username = $2, password_hash = $3, updated_at = NOW()
         WHERE admin_user_id = $1`,
        [adminUserId, username, passwordHash],
      );
      await client.query(
        `UPDATE mira_admin_sessions SET revoked_at = NOW()
         WHERE admin_user_id = $1 AND revoked_at IS NULL`,
        [adminUserId],
      );
    }
    await client.query("COMMIT");
    process.stdout.write(`Mira administrator ${username} is configured.\n`);
  } catch (error) {
    await client.query("ROLLBACK");
    throw error;
  } finally {
    client.release();
  }
}

const [command, username = "admin", ...extra] = process.argv.slice(2);
if (command !== "set-password" || extra.length > 0) {
  process.stderr.write("Usage: npm run admin -- set-password [username]\n");
  process.exit(2);
}

const databaseUrl =
  process.env.DATABASE_URL ?? "postgresql://mira:mira-local@127.0.0.1:55432/mira";
const pool = new Pool({ connectionString: databaseUrl, max: 2 });
try {
  await initializeDatabase(pool);
  await setPassword(pool, username);
} catch (error) {
  process.stderr.write(`Could not configure Mira administrator: ${error.message}\n`);
  process.exitCode = 1;
} finally {
  await pool.end();
}
