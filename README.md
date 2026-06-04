# pg-backup

Automated PostgreSQL backup tool with cloud storage, retention policies, and failure monitoring.

It dumps your PostgreSQL database every night, compresses it, uploads it to S3-compatible cloud storage (Cloudflare R2 or Backblaze B2), rotates old backups automatically, and alerts you if anything goes wrong.

---

## Features

- Daily backups via `pg_dump`, compressed with gzip
- Grandfather-Father-Son (GFS) retention — daily, weekly, monthly tiers
- Works with any S3-compatible provider (Cloudflare R2, Backblaze B2)
- Layer 1 alerting — POSTs a failure event to a webhook (e.g. NotifyKit) if the backup breaks
- Layer 2 monitoring — pings healthchecks.io on success; silence triggers an alarm if the job stops running entirely
- Single self-contained binary
- Fully configured via environment variables

---

## How it works

```
cron (03:00)
    └── run()
          ├── dumpAndCompress()   pg_dump | gzip → temp file
          ├── upload()            temp file → S3-compatible storage (daily/)
          ├── copyObject()        promote to weekly/ or monthly/ if applicable
          └── rotate()            delete files older than retention window
    └── ping()                    notify healthchecks.io on success
    └── alert()                   POST failure event to webhook on error
```

---

## Retention policy

| Tier        | Created      | Kept for | Env var                  |
| ----------- | ------------ | -------- | ------------------------ |
| **Daily**   | Every night  | 14 days  | `RETENTION_DAYS`         |
| **Weekly**  | Sundays      | 8 weeks  | `WEEKLY_RETENTION_DAYS`  |
| **Monthly** | 1st of month | 6 months | `MONTHLY_RETENTION_DAYS` |

Weekly and monthly backups are server-side copies of the daily.

---

## Code structure

```
main.go
├── cfg                  Struct holding all configuration values
├── loadCfg()            Reads config from environment variables
├── main()               Entry point
├── run()                Orchestrates the full backup sequence
├── dumpAndCompress()    Streams pg_dump output through gzip into a temp file
├── s3Client()           Builds the S3 client from config
├── upload()             Uploads the compressed file to cloud storage
├── copyObject()         Server-side copy for weekly/monthly promotion
├── rotate()             Lists and deletes files older than retention window
├── alert()              Layer 1 — POSTs failure event to webhook
├── ping()               Layer 2 — Sends heartbeat to healthchecks.io
└── helpers              getenv(), mustenv(), atoi(), sizeOf()
```

---

## Environment variables

Create `sudo nano /etc/your-app-backup.env` and populate it:

```bash
# Database
PG_CONTAINER=your-app-postgres     # Docker container name running Postgres
PG_USER=your-app                   # Postgres user
PG_DB=your-app                     # Database name

# Cloud storage
S3_BUCKET=your-app-db-backups      # Bucket name
S3_ENDPOINT=https://...            # R2: https://<accountid>.r2.cloudflarestorage.com
                                   # B2: https://s3.<region>.backblazeb2.com
S3_REGION=auto                     # "auto" for R2, region code for B2 e.g. us-west-004
S3_ACCESS_KEY=                     # R2: Access Key ID / B2: keyID
S3_SECRET_KEY=                     # R2: Secret Access Key / B2: applicationKey

# Bucket folders (optional — these are the defaults)
S3_DAILY_PREFIX=daily/
S3_WEEKLY_PREFIX=weekly/
S3_MONTHLY_PREFIX=monthly/

# Retention (in days)
RETENTION_DAYS=14
WEEKLY_RETENTION_DAYS=56
MONTHLY_RETENTION_DAYS=186

# Layer 1 — failure alert via NotifyKit
NOTIFYKIT_API_URL=https://api.notifykit.dev/api/v1/notifications/webhook  # NotifyKit receives the failure event
NOTIFYKIT_API_KEY=your-notifykit-api-key                                       # your NotifyKit API key
ALERT_WEBHOOK_URL=https://discord.com/api/webhooks/...                 # where NotifyKit forwards the alert to (Discord, Slack, etc.)

# Layer 2 — healthchecks.io heartbeat
HEALTHCHECK_URL=https://hc-ping.com/your-check-uuid
```

Lock down the env file so only root can read it:

```bash
sudo chmod 600 /etc/your-app-backup.env
```

---

## Setup

### 1. Install Go

```bash
# Check if Go is already installed
go version
```

If Go isn't installed:

**macOS**

```bash
brew install go
```

**Ubuntu/Debian**

```bash
sudo apt update && sudo apt install -y golang-go
```

Verify the installation:

```bash
go version
```

### 2. Clone the repo

```bash
git clone https://github.com/Brayzonn/vps-pg-backup.git
cd vps-pg-backup
```

### 3. Pull dependencies

```bash
go mod download
```

### 4. Build

```bash
# On Linux:
go build -o pg-backup .

# On macOS targeting a Linux VPS:
GOOS=linux GOARCH=amd64 go build -o pg-backup .
```

### 5. Copy to the VPS

Copy the compiled binary to the VPS, move it into /usr/local/bin, and make it executable so it can be run from anywhere on the system as pg-backup.

```bash
scp pg-backup user@your-vps:/tmp/
ssh user@your-vps 'sudo mv /tmp/pg-backup /usr/local/bin/ && sudo chmod +x /usr/local/bin/pg-backup'
```

### 6. Test manually

```bash
sudo bash -c 'set -a; . /etc/your-app-backup.env; set +a; pg-backup'
```

You should see `backup OK` in the output. Confirm the file appears in your bucket and the healthchecks.io check turns green.

### 7. Schedule with cron

```bash
sudo crontab -e
```

Add this line (runs daily at 03:00):

```
0 3 * * * bash -c 'set -a && source /etc/your-app-backup.env && /usr/local/bin/pg-backup' >> /var/log/pg-backup.log 2>&1
```

Check your server timezone with `timedatectl` so 03:00 means what you expect.

---

## Monitoring

**Layer 1 — Webhook alert**
If the binary runs but something breaks (upload fails, dump is empty, etc.), it sends a `backup_failed` event to NotifyKit. NotifyKit forwards the alert to your configured webhook destination and logs the event.

**Layer 2 — healthchecks.io**
The binary pings `HEALTHCHECK_URL` on every successful backup. If healthchecks.io doesn't receive a ping within 25 hours, it fires an alert. This catches failures Layer 1 can't — VPS off, cron misconfigured, binary crashed before reaching the alert code.

Set up a free check at [healthchecks.io](https://healthchecks.io): Period = 1 day, Grace = 1 hour.

---

## Restore

Both Cloudflare R2 and Backblaze B2 are S3-compatible, so the AWS CLI works with either one. Install it once and use it for all downloads.

### 1. Install the AWS CLI

**macOS**

```bash
brew install awscli
```

**Ubuntu/Debian**

```bash
# Install dependencies:
sudo apt install -y unzip curl

# Download and install:
curl "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o "awscliv2.zip"
unzip awscliv2.zip
sudo ./aws/install
```

Verify:

```bash
aws --version
```

### 2. Configure it for your provider

The AWS CLI needs your credentials and endpoint. Run this once:

**Cloudflare R2/Backblaze B2**

```bash
sudo -E bash -c 'set -a && source /etc/your-app-backup.env && aws configure set aws_access_key_id $S3_ACCESS_KEY && aws configure set aws_secret_access_key $S3_SECRET_KEY'
```

### 3. Download a backup

```bash
# List available backups:
sudo -E bash -c 'set -a && source /etc/your-app-backup.env && aws --endpoint-url $S3_ENDPOINT s3 ls s3://$S3_BUCKET/daily/'

# Download the one you want:
sudo -E bash -c 'set -a && source /etc/your-app-backup.env && aws --endpoint-url $S3_ENDPOINT s3 cp s3://$S3_BUCKET/daily/your-app_2026-05-25_030000.sql.gz /tmp/'
```

Replace the filename with the actual backup you want to restore.

### Testing a backup (restore drill)

Run this monthly to confirm your backups are actually usable. After downloading the file to `/tmp/`, spin up a throwaway Postgres container and restore into it:

```bash
# Spin up a throwaway Postgres container:
docker run --name pg-test -e POSTGRES_PASSWORD=test -d postgres:16

# Restore into it:
gunzip -c /tmp/your-app_2026-05-25_030000.sql.gz \
  | docker exec -i pg-test psql -U postgres

# Sanity check:
docker exec -i pg-test psql -U postgres -c "SELECT count(*) FROM customers;"

# Clean up:
docker rm -f pg-test
```

### Actual disaster recovery

If your VPS is gone and you need to fully restore into production, download the backup file first using the steps above, copy it to your new VPS, then stop your app so nothing writes mid-restore:

```bash
docker compose stop api

gunzip -c /tmp/your-app_2026-05-25_030000.sql.gz \
  | docker exec -i your-app-postgres psql -U your-app -d your-app

docker compose start api
```

> Practice the drill at least once before you need it. A backup you have never restored is just a hopeful guess :)

---

## Read more

A full walkthrough of how this works, including the monitoring setup and retention strategy is on [my blog](https://zoneyhub.com/blog/automating-postgresql-backups-with-cloudflare-r2).

---

## License

[MIT](./LICENSE) © 2026 Eyinda Bright
