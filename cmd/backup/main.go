// Command kuu-backup is a long-running daemon that periodically pg_dumps the
// kuu-api database, gzips the result, uploads it to an S3-compatible bucket,
// and deletes objects older than the configured retention window.
//
// It is deployed as a separate docker-compose service (gated behind the
// "backup" profile) rather than baked into kuu-api so that:
//   - The backup container has pg_dump available (it's based on postgres:16),
//     while the API image stays distroless and minimal.
//   - Backup failures don't crash the API.
//   - Operators can start backups only after S3 creds are provisioned, by
//     enabling the profile.
//
// Required env vars:
//
//	DATABASE_URL                     Postgres connection string
//	BACKUP_S3_ENDPOINT               e.g. https://s3.us-west-002.backblazeb2.com
//	BACKUP_S3_BUCKET                 destination bucket
//	BACKUP_S3_ACCESS_KEY             credential key
//	BACKUP_S3_SECRET_KEY             credential secret
//
// Optional:
//
//	BACKUP_S3_REGION                 default: us-east-1 (most providers ignore)
//	BACKUP_S3_PREFIX                 default: kuu/ (key prefix inside bucket)
//	BACKUP_INTERVAL_HOURS            default: 24
//	BACKUP_RETENTION_DAYS            default: 30
//	BACKUP_RUN_ON_START              default: true (do one immediately, then tick)
//	BACKUP_S3_USE_SSL                default: true
package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const objectKeyTimeFormat = "2006-01-02T150405Z"

type config struct {
	DatabaseURL   string
	S3Endpoint    string
	S3Bucket      string
	S3AccessKey   string
	S3SecretKey   string
	S3Region      string
	S3Prefix      string
	S3UseSSL      bool
	IntervalHours int
	RetentionDays int
	RunOnStart    bool
}

func loadConfig() (*config, error) {
	cfg := &config{
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		S3Endpoint:    os.Getenv("BACKUP_S3_ENDPOINT"),
		S3Bucket:      os.Getenv("BACKUP_S3_BUCKET"),
		S3AccessKey:   os.Getenv("BACKUP_S3_ACCESS_KEY"),
		S3SecretKey:   os.Getenv("BACKUP_S3_SECRET_KEY"),
		S3Region:      getEnvDefault("BACKUP_S3_REGION", "us-east-1"),
		S3Prefix:      getEnvDefault("BACKUP_S3_PREFIX", "kuu/"),
		IntervalHours: getEnvInt("BACKUP_INTERVAL_HOURS", 24),
		RetentionDays: getEnvInt("BACKUP_RETENTION_DAYS", 30),
		RunOnStart:    getEnvBool("BACKUP_RUN_ON_START", true),
		S3UseSSL:      getEnvBool("BACKUP_S3_USE_SSL", true),
	}

	missing := []string{}
	if cfg.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if cfg.S3Endpoint == "" {
		missing = append(missing, "BACKUP_S3_ENDPOINT")
	}
	if cfg.S3Bucket == "" {
		missing = append(missing, "BACKUP_S3_BUCKET")
	}
	if cfg.S3AccessKey == "" {
		missing = append(missing, "BACKUP_S3_ACCESS_KEY")
	}
	if cfg.S3SecretKey == "" {
		missing = append(missing, "BACKUP_S3_SECRET_KEY")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	if !strings.HasSuffix(cfg.S3Prefix, "/") && cfg.S3Prefix != "" {
		cfg.S3Prefix += "/"
	}
	if cfg.IntervalHours < 1 {
		return nil, fmt.Errorf("BACKUP_INTERVAL_HOURS must be >= 1")
	}
	if cfg.RetentionDays < 1 {
		return nil, fmt.Errorf("BACKUP_RETENTION_DAYS must be >= 1")
	}
	return cfg, nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	endpointHost, secure, err := parseEndpoint(cfg.S3Endpoint, cfg.S3UseSSL)
	if err != nil {
		log.Fatalf("parse BACKUP_S3_ENDPOINT: %v", err)
	}

	client, err := minio.New(endpointHost, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		Secure: secure,
		Region: cfg.S3Region,
	})
	if err != nil {
		log.Fatalf("init s3 client: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Verify the bucket is reachable before entering the loop. Any error is
	// fatal — better to crash and surface the problem than silently fail
	// every backup for hours.
	if exists, err := client.BucketExists(ctx, cfg.S3Bucket); err != nil {
		log.Fatalf("bucket check failed: %v", err)
	} else if !exists {
		log.Fatalf("bucket %q does not exist or credentials lack access", cfg.S3Bucket)
	}
	log.Printf("kuu-backup ready: bucket=%s endpoint=%s interval=%dh retention=%dd",
		cfg.S3Bucket, endpointHost, cfg.IntervalHours, cfg.RetentionDays)

	interval := time.Duration(cfg.IntervalHours) * time.Hour
	if cfg.RunOnStart {
		if err := runOnce(ctx, cfg, client); err != nil {
			log.Printf("initial backup failed: %v", err)
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("shutdown requested, exiting")
			return
		case <-ticker.C:
			if err := runOnce(ctx, cfg, client); err != nil {
				log.Printf("backup failed: %v", err)
			}
		}
	}
}

// runOnce performs a single dump + upload + prune cycle. Returns the first
// error encountered; the caller logs it. Continuing after a failure is
// fine — next tick will try again.
func runOnce(ctx context.Context, cfg *config, client *minio.Client) error {
	now := time.Now().UTC()
	start := time.Now()
	objectKey := fmt.Sprintf("%skuu-%s.sql.gz", cfg.S3Prefix, now.Format(objectKeyTimeFormat))

	log.Printf("backup starting key=%s", objectKey)

	tmpFile, err := os.CreateTemp("", "kuu-backup-*.sql.gz")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if err := dumpToFile(ctx, cfg.DatabaseURL, tmpFile); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}

	stat, err := os.Stat(tmpPath)
	if err != nil {
		return fmt.Errorf("stat temp: %w", err)
	}

	uploadFile, err := os.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("open temp for upload: %w", err)
	}
	defer uploadFile.Close()

	uploadInfo, err := client.PutObject(ctx, cfg.S3Bucket, objectKey, uploadFile, stat.Size(),
		minio.PutObjectOptions{ContentType: "application/gzip"})
	if err != nil {
		return fmt.Errorf("upload to s3: %w", err)
	}

	log.Printf("backup uploaded key=%s bytes=%d duration=%s",
		uploadInfo.Key, uploadInfo.Size, time.Since(start).Round(time.Millisecond))

	deleted, err := pruneOldBackups(ctx, cfg, client, now)
	if err != nil {
		log.Printf("prune failed (non-fatal): %v", err)
	} else if deleted > 0 {
		log.Printf("prune deleted %d old object(s)", deleted)
	}
	return nil
}

// dumpToFile streams `pg_dump | gzip -c` into the given file. We invoke
// gzip via the shell rather than wrapping with Go's compress/gzip so a
// crash mid-write leaves a partial file with the right extension that we
// can still inspect. The temp file is the upload source.
func dumpToFile(ctx context.Context, databaseURL string, dst *os.File) error {
	dumpCmd := exec.CommandContext(ctx, "pg_dump", "--no-owner", "--no-acl", databaseURL)
	gzipCmd := exec.CommandContext(ctx, "gzip", "-c")

	pipe, err := dumpCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pg_dump stdout pipe: %w", err)
	}
	gzipCmd.Stdin = pipe
	gzipCmd.Stdout = dst

	var dumpErr, gzipErr strings.Builder
	dumpCmd.Stderr = &dumpErr
	gzipCmd.Stderr = &gzipErr

	if err := gzipCmd.Start(); err != nil {
		return fmt.Errorf("start gzip: %w", err)
	}
	if err := dumpCmd.Run(); err != nil {
		_ = gzipCmd.Wait()
		return fmt.Errorf("pg_dump failed: %w (stderr: %s)", err, strings.TrimSpace(dumpErr.String()))
	}
	if err := gzipCmd.Wait(); err != nil {
		return fmt.Errorf("gzip failed: %w (stderr: %s)", err, strings.TrimSpace(gzipErr.String()))
	}
	return nil
}

// pruneOldBackups deletes objects whose key embeds a timestamp older than
// the retention cutoff. Listing-based pruning is preferred over LastModified
// so a panel-level clock skew can't accidentally delete a recent backup.
func pruneOldBackups(ctx context.Context, cfg *config, client *minio.Client, now time.Time) (int, error) {
	cutoff := now.Add(-time.Duration(cfg.RetentionDays) * 24 * time.Hour)
	deleted := 0
	for object := range client.ListObjects(ctx, cfg.S3Bucket, minio.ListObjectsOptions{
		Prefix:    cfg.S3Prefix,
		Recursive: true,
	}) {
		if object.Err != nil {
			return deleted, fmt.Errorf("list: %w", object.Err)
		}
		ts, ok := timestampFromKey(object.Key, cfg.S3Prefix)
		if !ok {
			continue
		}
		if ts.After(cutoff) {
			continue
		}
		if err := client.RemoveObject(ctx, cfg.S3Bucket, object.Key, minio.RemoveObjectOptions{}); err != nil {
			return deleted, fmt.Errorf("remove %s: %w", object.Key, err)
		}
		deleted++
	}
	return deleted, nil
}

// timestampFromKey parses the timestamp embedded in an object key shaped
// like "<prefix>kuu-<RFC3339-ish>.sql.gz". Returning ok=false means the
// key doesn't match our naming, so we leave it alone.
func timestampFromKey(key, prefix string) (time.Time, bool) {
	rest := strings.TrimPrefix(key, prefix)
	if !strings.HasPrefix(rest, "kuu-") || !strings.HasSuffix(rest, ".sql.gz") {
		return time.Time{}, false
	}
	stamp := strings.TrimSuffix(strings.TrimPrefix(rest, "kuu-"), ".sql.gz")
	t, err := time.Parse(objectKeyTimeFormat, stamp)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// parseEndpoint accepts either a full URL ("https://s3.example.com") or a
// bare host ("s3.example.com"), returning host:port and whether to use TLS.
// minio-go expects the bare form, so we strip the scheme but use it as a
// hint for the Secure flag, falling back to BACKUP_S3_USE_SSL otherwise.
func parseEndpoint(raw string, defaultSSL bool) (host string, secure bool, err error) {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "://") {
		u, perr := url.Parse(raw)
		if perr != nil {
			return "", false, perr
		}
		secure = u.Scheme == "https"
		host = u.Host
		if host == "" {
			return "", false, fmt.Errorf("endpoint missing host: %q", raw)
		}
		return host, secure, nil
	}
	return raw, defaultSSL, nil
}

func getEnvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "":
		return fallback
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return fallback
}
