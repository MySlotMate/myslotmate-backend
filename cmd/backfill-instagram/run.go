// backfill-instagram runs the one-time Instagram media scrape for EXISTING
// hosts who applied before the feature existed: those with no profile photo,
// an Instagram link, and no prior scrape. It re-hosts their Instagram profile
// photo (as the avatar) and up to 3 recent post photos (gallery) on S3.
//
// Run from the backend root (needs DATABASE_URL + AWS_S3_* in .env):
//
//	go run ./cmd/backfill-instagram          # scrape all eligible hosts
//	go run ./cmd/backfill-instagram -dry-run # list who would be scraped, do nothing
//
// Best-effort per host: a failure is logged and the run continues. Instagram
// rate-limits datacenter IPs, so profile photos are the reliable win; recent
// posts often come back empty on unauthenticated calls.
package main

import (
	"context"
	"flag"
	"log"
	"time"

	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/joho/godotenv"

	"myslotmate-backend/internal/config"
	"myslotmate-backend/internal/db"
	"myslotmate-backend/internal/lib/storage"
	"myslotmate-backend/internal/repository"
	"myslotmate-backend/internal/service"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "list eligible hosts without scraping")
	delay := flag.Duration("delay", 5*time.Second, "pause between hosts to avoid rate limits")
	flag.Parse()

	if err := godotenv.Load(".env"); err != nil {
		log.Printf("warning: could not load .env: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	if cfg.Database.URL == "" {
		log.Fatal("DATABASE_URL is not set")
	}

	ctx := context.Background()
	sqlDB, err := db.OpenWithContext(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("db connection failed: %v", err)
	}
	defer sqlDB.Close()

	hostRepo := repository.NewHostRepository(sqlDB)

	hosts, err := hostRepo.ListNeedingInstagramScrape(ctx)
	if err != nil {
		log.Fatalf("failed to list hosts: %v", err)
	}
	log.Printf("found %d host(s) eligible for Instagram backfill", len(hosts))
	if len(hosts) == 0 {
		return
	}

	if *dryRun {
		for _, h := range hosts {
			ig := ""
			if h.SocialInstagram != nil {
				ig = *h.SocialInstagram
			}
			log.Printf("[dry-run] host=%s name=%s %s instagram=%s", h.ID, h.FirstName, h.LastName, ig)
		}
		log.Printf("dry run: %d host(s) would be scraped", len(hosts))
		return
	}

	// S3 upload service — required to re-host the images.
	if cfg.S3.Bucket == "" || cfg.S3.AccessKey == "" || cfg.S3.SecretKey == "" {
		log.Fatal("AWS S3 not configured (need AWS_S3_BUCKET, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY) — cannot re-host images")
	}
	awsCfg, err := awscfg.LoadDefaultConfig(ctx,
		awscfg.WithRegion(cfg.S3.Region),
		awscfg.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.S3.AccessKey, cfg.S3.SecretKey, "",
		)),
	)
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}
	uploads := storage.NewUploadService(s3.NewFromConfig(awsCfg), cfg.S3.Bucket, cfg.S3.Region)

	var scraped, failed int
	for i, h := range hosts {
		if i > 0 {
			time.Sleep(*delay)
		}
		hctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		avatarSet, gallery, err := service.ScrapeInstagramMedia(hctx, uploads, hostRepo, h)
		cancel()
		if err != nil {
			log.Printf("host=%s: FAILED: %v", h.ID, err)
			failed++
			continue
		}
		log.Printf("host=%s: OK avatar=%t gallery=%d", h.ID, avatarSet, gallery)
		scraped++
	}
	log.Printf("backfill complete: %d scraped, %d failed, %d total", scraped, failed, len(hosts))
}
