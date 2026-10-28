package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"doriyamedia/internal/api"
	"doriyamedia/internal/config"
	"doriyamedia/internal/db"
	"doriyamedia/internal/media"
	"doriyamedia/internal/metadata"
	"doriyamedia/internal/models"
	"doriyamedia/internal/scan"
	"doriyamedia/internal/site"
	"doriyamedia/internal/source"
	"doriyamedia/internal/web"
)

// version 由构建时注入：
//
//	go build -ldflags "-X main.version=v1.2.3" ./cmd/doriyamedia
var version = "dev"

func main() {
	cfgPath := flag.String("config", "config.json", "config file path")
	port := flag.Int("port", 0, "override listen port")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("doriyamedia " + version)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if *port > 0 {
		cfg.Port = *port
	}
	if err := os.MkdirAll(cfg.ImagesDir(), 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	store, err := db.Open(cfg.DBPath())
	if err != nil {
		log.Fatalf("open db: %v", err)
	}

	engine := media.NewEngine(store, cfg)
	// Adopt packaged output a previous run left behind (and drop anything that
	// never finished), so replaying a file does not package it again.
	engine.LoadCache()
	manager := metadata.NewManager(store, cfg)
	scanner := &scan.Scanner{
		Store: store,
		Cfg:   cfg,
		OnIdentify: func(lib *models.Library, listings map[string][]source.Entry, report func(done, total int, msg string)) {
			manager.SeedListings(listings)
			manager.EnrichLibrary(lib, report)
		},
		OnIdentifyFiles: func(lib *models.Library, files []models.MediaFile, listings map[string][]source.Entry, report func(done, total int, msg string)) {
			manager.SeedListings(listings)
			manager.EnrichFiles(lib, files, report)
		},
	}
	// Probe is the slowest step of starting playback on a cloud drive. Warm the
	// cache in the background after each scan so opening a file is already
	// decided; bounded and rate-limited so it never competes with playback.
	scanner.OnScanned = func(lib *models.Library) {
		if !cfg.Playback.PreProbeAfterScan {
			return
		}
		files, err := store.FilesWithoutProbe(lib.ID, cfg.Playback.PreProbeMaxFiles)
		if err != nil {
			log.Printf("pre-probe: list files for library %d failed: %v", lib.ID, err)
			return
		}
		engine.PreProbeFiles(files)
	}
	scanner.OnScannedFiles = func(lib *models.Library, files []models.MediaFile) {
		if !cfg.Playback.PreProbeAfterScan {
			return
		}
		var pending []*models.MediaFile
		for i := range files {
			if files[i].Probe == "" {
				pending = append(pending, &files[i])
				if cfg.Playback.PreProbeMaxFiles > 0 && len(pending) >= cfg.Playback.PreProbeMaxFiles {
					break
				}
			}
		}
		engine.PreProbeFiles(pending)
	}

	srv := api.New(store, cfg, scanner, manager, engine, web.Dist(), version)
	srv.ConfigPath = *cfgPath
	siteStore, err := site.Open(cfg.SitePath())
	if err != nil {
		log.Fatalf("open site content: %v", err)
	}
	srv.Site = siteStore
	srv.BangumiSync.Start()

	addr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	if os.Getenv("DORIYAMEDIA_LOCAL") == "1" {
		addr = fmt.Sprintf("127.0.0.1:%d", cfg.Port) // 仅本机访问
	}
	log.Printf("===========================================")
	log.Printf("  DoriyaMedia %s  →  http://%s", version, addr)
	log.Printf("  首次使用请先打开网页完成管理员初始化")
	log.Printf("===========================================")
	httpServer := &http.Server{Addr: addr, Handler: srv.Handler()}
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
