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
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"unspok3n/beatportdl/config"
	"unspok3n/beatportdl/internal/beatport"
)

const (
	// Required by utils.go
	configFilename = "beatportdl-config.yml"
	errorFilename  = "beatportdl-err.log"
)

type account struct {
	cfg *config.AppConfig
	bp  *beatport.Beatport
	bs  *beatport.Beatport
}

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

var (
	accounts      []*account
	activeAccount int
	accountMutex  sync.Mutex
)

func main() {

	configDir := "/home/ubuntu/.config/beatportdl"

	entries, err := os.ReadDir(configDir)
	if err != nil {
		fmt.Println("Failed to read config directory:", err)
		os.Exit(1)
	}

	// 🔹 Load ALL accounts
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yml") {
			continue
		}

		cfgPath := filepath.Join(configDir, entry.Name())
		cfg, err := config.Parse(cfgPath)
		if err != nil {
			continue
		}

		auth := beatport.NewAuth(cfg.Username, cfg.Password, "") // NO CACHE
		bp := beatport.New(beatport.StoreBeatport, cfg.Proxy, auth)
		bs := beatport.New(beatport.StoreBeatsource, cfg.Proxy, auth)

		if err := auth.Init(bp); err != nil {
			fmt.Println("❌ Login failed:", entry.Name())
			continue
		}

		fmt.Println("✅ Loaded account:", entry.Name())
		accounts = append(accounts, &account{
			cfg: cfg,
			bp:  bp,
			bs:  bs,
		})
	}

	if len(accounts) == 0 {
		fmt.Println("❌ No valid accounts available")
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())

	app := &application{
		config:      accounts[0].cfg,
		bp:          accounts[0].bp,
		bs:          accounts[0].bs,
		downloadSem: make(chan struct{}, accounts[0].cfg.MaxDownloadWorkers),
		globalSem:   make(chan struct{}, accounts[0].cfg.MaxGlobalWorkers),
		ctx:         ctx,
		logWriter:   os.Stdout,
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

		<-sigCh
		if len(app.urls) > 0 {
			app.LogInfo("Shutdown signal received. Waiting for download workers to finish")
			cancel()
			<-sigCh
		}
		os.Exit(0)
	}()

	if app.config.WriteErrorLog {
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

	quitFlag := flag.Bool("q", false, "Quit after finishing")
	flag.Parse()

	for _, arg := range flag.Args() {
		if strings.HasSuffix(arg, ".txt") {
			app.parseTextFile(arg)
		} else {
			app.urls = append(app.urls, arg)
		}
	}

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

// 🔁 Auto account switcher
func (app *application) switchAccount() bool {
	accountMutex.Lock()
	defer accountMutex.Unlock()

	if len(accounts) < 2 {
		return false
	}

	activeAccount = (activeAccount + 1) % len(accounts)

	app.config = accounts[activeAccount].cfg
	app.bp = accounts[activeAccount].bp
	app.bs = accounts[activeAccount].bs

	app.LogInfo("🔁 Switched to next account")
	return true
}
