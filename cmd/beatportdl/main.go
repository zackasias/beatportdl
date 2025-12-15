package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/fatih/color"
	"github.com/vbauerster/mpb/v8"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"unspok3n/beatportdl/config"
	"unspok3n/beatportdl/internal/beatport"
)

const (
	// KEEP this to satisfy utils.go, but cache is DISABLED
	cacheFilename = ""
	errorFilename = "beatportdl-err.log"
)

type application struct {
	config      *config.AppConfig
	logFile     *os.File
	logWriter   io.Writer
	ctx         context.Context
	wg          sync.WaitGroup
	downloadSem chan struct{}
	globalSem   chan struct{}
	pbp         *mpb.Progress

	urls             []string
	activeFiles      map[string]struct{}
	activeFilesMutex sync.RWMutex

	bp *beatport.Beatport
	bs *beatport.Beatport
}

func main() {

	// === MULTIPLE ACCOUNT CONFIG FILES ===
	configFiles := []string{
		"/home/ubuntu/.config/beatportdl/config1.yml",
		"/home/ubuntu/.config/beatportdl/config2.yml",
		"/home/ubuntu/.config/beatportdl/config3.yml",
	}

	var (
		cfg          *config.AppConfig
		auth         *beatport.Auth
		bp, bs       *beatport.Beatport
		loginSuccess bool
	)

	// === TRY EACH ACCOUNT UNTIL LOGIN SUCCESS ===
	for _, cfgPath := range configFiles {

		var err error
		cfg, err = config.Parse(cfgPath)
		if err != nil {
			fmt.Println("Config error:", cfgPath, err)
			continue
		}

		// CACHE PATH EMPTY → NO JSON CREATED
		auth = beatport.NewAuth(cfg.Username, cfg.Password, "")

		bp = beatport.New(beatport.StoreBeatport, cfg.Proxy, auth)
		bs = beatport.New(beatport.StoreBeatsource, cfg.Proxy, auth)

		fmt.Println("Logging in:", cfgPath)

		// FORCE LIVE LOGIN (NO CACHE)
		if err := auth.Init(bp); err != nil {
			fmt.Println("Login failed:", cfgPath)
			continue
		}

		fmt.Println("Login successful with:", cfgPath)
		loginSuccess = true
		break
	}

	if !loginSuccess {
		fmt.Println("❌ All accounts failed. Exiting.")
		os.Exit(1)
	}

	// === CONTEXT ===
	ctx, cancel := context.WithCancel(context.Background())

	app := &application{
		config:      cfg,
		downloadSem: make(chan struct{}, cfg.MaxDownloadWorkers),
		globalSem:   make(chan struct{}, cfg.MaxGlobalWorkers),
		ctx:         ctx,
		logWriter:   os.Stdout,
		bp:          bp,
		bs:          bs,
	}

	// === SIGNAL HANDLING ===
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

		<-sigCh
		if len(app.urls) > 0 {
			app.LogInfo("Shutdown signal received, waiting for downloads...")
			cancel()
			<-sigCh
		}
		os.Exit(0)
	}()

	// === ERROR LOG ===
	if cfg.WriteErrorLog {
		logFilePath, _, err := FindErrorLogFile()
		if err != nil {
			fmt.Println(err.Error())
			Pause()
		}
		f, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
		if err != nil {
			panic(err)
		}
		app.logFile = f
		defer f.Close()
	}

	// === CLI ARGS ===
	quitFlag := flag.Bool("q", false, "Quit after finishing")
	flag.Parse()
	inputArgs := flag.Args()

	for _, arg := range inputArgs {
		if strings.HasSuffix(arg, ".txt") {
			app.parseTextFile(arg)
		} else {
			app.urls = append(app.urls, arg)
		}
	}

	// === MAIN LOOP ===
	for {
		if len(app.urls) == 0 {
			app.mainPrompt()
		}

		app.pbp = mpb.New(mpb.WithAutoRefresh(), mpb.WithOutput(color.Output))
		app.logWriter = app.pbp
		app.activeFiles = make(map[string]struct{}, len(app.urls))

		for _, url := range app.urls {
			app.globalWorker(func() {
				app.handleUrl(url)
			})
		}

		app.wg.Wait()
		app.pbp.Shutdown()

		if *quitFlag || ctx.Err() != nil {
			break
		}

		app.urls = []string{}
	}
}
