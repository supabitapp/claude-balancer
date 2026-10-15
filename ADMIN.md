# Browser admin

Open `/admin` for the authenticated dashboard with full account details,
client addresses, API key names, conversation identifiers, and event metadata.
The hidden route has no link from the public dashboard. Adding an account uses
the `/accounts` sign-in flow.

## Set or reset the password

Run this on the server:

```sh
claude-balancer admin password
```

Enter the new password twice at the hidden prompts. A long passphrase is
recommended. The same command resets a forgotten password without requiring
the previous password. Remote entry requires a terminal:

```sh
ssh -t balancer claude-balancer admin password
```

Choose another database or turn admin access off with:

```sh
claude-balancer admin password -state /path/to/state.db
claude-balancer admin disable
```

Passwords cannot be passed as command arguments or through a pipe. Setting or
resetting the password and disabling access take effect without restarting the
server. Existing admin sessions become invalid on their next request.

## Storage and access

The default `~/.claude-balancer/state.db` stores the password verifier in
`settings.admin_password_hash`. It contains a random salt and a PBKDF2-HMAC-SHA256
hash; the plaintext password is never stored. Database backups also contain
account credentials and API keys.

Admin access starts disabled until a password is set. Client inference API keys
do not grant admin access. Serve the admin interface over HTTPS; plain HTTP
cookies are allowed only for direct loopback development.

Admin sessions are stored in `admin_sessions` in the same database, so they
survive restarts and deploys and are shared by servers using that database.
Only a SHA-256 digest of each session token is stored. Cookies are host-only,
HttpOnly, Secure, and SameSite=Strict in production. Forms use CSRF tokens and
reject cross-site submissions. Login attempts are rate-limited across the
server to bound password-hashing work, including behind a reverse proxy.

## Controls

- Account routing uses the normal, priority, and paused selector on each row and
  saves on change. Paused retires the account's existing connections; normal or
  priority resumes routing. Removal also retires existing connections.
- New API key secrets appear only in the creation response. Existing keys remain
  valid until revoked. Key lists show names, status, dates, and usage, never
  existing secrets.
- The admin dashboard streams updates over `/admin/events`. API key usage totals
  refresh periodically and after key changes.

The UI reuses the dashboard's Go templates with HTMX. Full-page form submissions
also work without JavaScript; HTMX provides in-place updates, notices, and
confirmation dialogs.
