package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type cfg struct {
	Container string
	DBUser    string
	DBName    string

	Bucket    string
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string

	DailyPrefix   string
	WeeklyPrefix  string
	MonthlyPrefix string

	DailyKeep   int
	WeeklyKeep  int
	MonthlyKeep int

	NotifykitAPIURL    string
	NotifykitAPIKey string
	AlertWebhookURL string

	HealthcheckURL string
}

func loadCfg() cfg {
	return cfg{
		Container: getenv("PG_CONTAINER", "notifykit-postgres"),
		DBUser:    getenv("PG_USER", "notifykit"),
		DBName:    getenv("PG_DB", "notifykit"),

		Bucket:    mustenv("S3_BUCKET"),
		Endpoint:  mustenv("S3_ENDPOINT"),
		Region:    getenv("S3_REGION", "auto"),
		AccessKey: mustenv("S3_ACCESS_KEY"),
		SecretKey: mustenv("S3_SECRET_KEY"),

		DailyPrefix:   getenv("S3_DAILY_PREFIX", "daily/"),
		WeeklyPrefix:  getenv("S3_WEEKLY_PREFIX", "weekly/"),
		MonthlyPrefix: getenv("S3_MONTHLY_PREFIX", "monthly/"),

		DailyKeep:   atoi("RETENTION_DAYS", 14),
		WeeklyKeep:  atoi("WEEKLY_RETENTION_DAYS", 56),
		MonthlyKeep: atoi("MONTHLY_RETENTION_DAYS", 186),

		NotifykitAPIURL: os.Getenv("NOTIFYKIT_API_URL"),
		NotifykitAPIKey: os.Getenv("NOTIFYKIT_API_KEY"),
		AlertWebhookURL: os.Getenv("ALERT_WEBHOOK_URL"),

		HealthcheckURL: os.Getenv("HEALTHCHECK_URL"),
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	c := loadCfg()

	if err := run(context.Background(), c); err != nil {
		log.Printf("ERROR: %v", err)
		alert(c, err)
		if c.HealthcheckURL != "" {
			ping(c.HealthcheckURL + "/fail")
		}
		os.Exit(1)
	}

	ping(c.HealthcheckURL)
	log.Printf("backup OK")
}

func run(ctx context.Context, c cfg) error {
	now := time.Now().UTC()
	name := fmt.Sprintf("notifykit_%s.sql.gz", now.Format("2006-01-02_150405"))
	dailyKey := c.DailyPrefix + name

	tmp, err := dumpAndCompress(c)
	if err != nil {
		return fmt.Errorf("dump: %w", err)
	}
	defer os.Remove(tmp)

	info, err := os.Stat(tmp)
	if err != nil || info.Size() < 1024 {
		return fmt.Errorf("dump too small (%d bytes) — refusing to upload", sizeOf(info))
	}

	client := s3Client(ctx, c)
	if err := upload(ctx, client, c.Bucket, dailyKey, tmp); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	log.Printf("uploaded s3://%s/%s (%d bytes)", c.Bucket, dailyKey, info.Size())

	if now.Weekday() == time.Sunday {
		if err := copyObject(ctx, client, c.Bucket, dailyKey, c.WeeklyPrefix+name); err != nil {
			log.Printf("WARN: weekly promotion failed: %v", err)
		} else {
			log.Printf("promoted → %s%s", c.WeeklyPrefix, name)
		}
	}
	if now.Day() == 1 {
		if err := copyObject(ctx, client, c.Bucket, dailyKey, c.MonthlyPrefix+name); err != nil {
			log.Printf("WARN: monthly promotion failed: %v", err)
		} else {
			log.Printf("promoted → %s%s", c.MonthlyPrefix, name)
		}
	}

	rotate(ctx, client, c.Bucket, c.DailyPrefix, c.DailyKeep)
	rotate(ctx, client, c.Bucket, c.WeeklyPrefix, c.WeeklyKeep)
	rotate(ctx, client, c.Bucket, c.MonthlyPrefix, c.MonthlyKeep)

	return nil
}

func dumpAndCompress(c cfg) (string, error) {
	f, err := os.CreateTemp("", "notifykit_*.sql.gz")
	if err != nil {
		return "", err
	}
	defer f.Close()

	cmd := exec.Command("docker", "exec", "-t", c.Container,
		"pg_dump", "-U", c.DBUser, "-d", c.DBName, "--clean", "--if-exists")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	cmd.Stderr = os.Stderr

	gz := gzip.NewWriter(f)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	if _, err := io.Copy(gz, stdout); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("pg_dump exited: %w", err)
	}
	return f.Name(), nil
}

func s3Client(ctx context.Context, c cfg) *s3.Client {
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(c.Region),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, "")),
	)
	if err != nil {
		log.Fatalf("aws config: %v", err)
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(c.Endpoint)
		o.UsePathStyle = true 
	})
}

func upload(ctx context.Context, client *s3.Client, bucket, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   f,
	})
	return err
}

func copyObject(ctx context.Context, client *s3.Client, bucket, src, dst string) error {
	_, err := client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(bucket),
		CopySource: aws.String(bucket + "/" + src),
		Key:        aws.String(dst),
	})
	return err
}

func rotate(ctx context.Context, client *s3.Client, bucket, prefix string, keepDays int) {
	cutoff := time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour)
	p := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			log.Printf("WARN: rotate list %s: %v", prefix, err)
			return
		}
		for _, obj := range page.Contents {
			if obj.LastModified != nil && obj.LastModified.Before(cutoff) {
				if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
					Bucket: aws.String(bucket),
					Key:    obj.Key,
				}); err != nil {
					log.Printf("WARN: rotate delete %s: %v", *obj.Key, err)
					continue
				}
				log.Printf("rotated out %s", *obj.Key)
			}
		}
	}
}

func alert(c cfg, cause error) {
	if c.NotifykitAPIURL == "" || c.NotifykitAPIKey == "" || c.AlertWebhookURL == "" {
		return
	}

	host, _ := os.Hostname()

	body, _ := json.Marshal(map[string]any{
		"url":    c.AlertWebhookURL, 
		"priority": 1,              
		"payload": map[string]any{
			"event": "backup_failed",
			"host":  host,
			"error": cause.Error(),
			"at":    time.Now().UTC().Format(time.RFC3339),
		},
	})

	req, err := http.NewRequest(http.MethodPost, c.NotifykitAPIURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.NotifykitAPIKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("WARN: alert webhook failed: %v", err)
		return
	}
	resp.Body.Close()
}

func ping(url string) {
	if url == "" {
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		log.Printf("WARN: healthcheck ping failed: %v", err)
		return
	}
	resp.Body.Close()
}


/**
* helpers
**/

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mustenv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("missing required env var %s", k)
	}
	return v
}

func atoi(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func sizeOf(i os.FileInfo) int64 {
	if i == nil {
		return 0
	}
	return i.Size()
}