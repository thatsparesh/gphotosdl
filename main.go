// Package main implements gphotosdl
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

const (
	program       = "gphotosdl"
	gphotosURL    = "https://photos.google.com/"
	loginURL      = "https://photos.google.com/login"
	gphotoURLReal = "https://photos.google.com/photo/"
	gphotoURL     = "https://photos.google.com/lr/photo/" // redirects to gphotosURLReal which uses a different ID
	photoID       = "AF1QipNJVLe7d5mOh-b4CzFAob1UW-6EpFd0HnCBT3c6"
)

// Flags
var (
	debug   = flag.Bool("debug", false, "set to see debug messages")
	login   = flag.Bool("login", false, "set to launch login browser")
	show    = flag.Bool("show", false, "set to show the browser (not headless)")
	addr    = flag.String("addr", "localhost:8282", "address for the web server")
	useJSON = flag.Bool("json", false, "log in JSON format")
)

// Global variables
var (
	configRoot    string      // top level config dir, typically ~/.config/gphotodl
	browserConfig string      // work directory for browser instance
	browserPath   string      // path to the browser binary
	downloadDir   string      // temporary directory for downloads
	browserPrefs  string      // JSON config for the browser
	version       = "DEV"     // set by goreleaser
	commit        = "NONE"    // set by goreleaser
	date          = "UNKNOWN" // set by goreleaser
)

// Remove the download directory and contents
func removeDownloadDirectory() {
	if downloadDir == "" {
		return
	}
	err := os.RemoveAll(downloadDir)
	if err == nil {
		slog.Debug("Removed download directory")
	} else {
		slog.Error("Failed to remove download directory", "err", err)
	}
}

// Set up the global variables from the flags
func config() (err error) {
	version := fmt.Sprintf("%s version %s, commit %s, built at %s", program, version, commit, date)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n%s\n", version)
	}
	flag.Parse()

	// Set up the logger
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	if *useJSON {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
		slog.SetDefault(logger)
	} else {
		slog.SetLogLoggerLevel(level) // set log level of Default Handler
	}
	slog.Debug(version)

	configRoot, err = os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("didn't find config directory: %w", err)
	}
	configRoot = filepath.Join(configRoot, program)
	browserConfig = filepath.Join(configRoot, "browser")
	err = os.MkdirAll(browserConfig, 0700)
	if err != nil {
		return fmt.Errorf("config directory creation: %w", err)
	}
	slog.Debug("Configured config", "config_root", configRoot, "browser_config", browserConfig)

	downloadDir, err = os.MkdirTemp("", program)
	if err != nil {
		log.Fatal(err)
	}
	slog.Debug("Created download directory", "download_directory", downloadDir)

	// Find the browser
	var ok bool
	browserPath, ok = launcher.LookPath()
	if !ok {
		return errors.New("browser not found")
	}
	slog.Debug("Found browser", "browser_path", browserPath)

	// Browser preferences
	pref := map[string]any{
		"download": map[string]any{
			"default_directory": "/tmp/gphotos", // FIXME
		},
	}
	prefJSON, err := json.Marshal(pref)
	if err != nil {
		return fmt.Errorf("failed to make preferences: %w", err)
	}
	browserPrefs = string(prefJSON)
	slog.Debug("made browser preferences", "prefs", browserPrefs)

	return nil
}

// logger makes an io.Writer from slog.Debug
type logger struct{}

// Write writes len(p) bytes from p to the underlying data stream.
func (logger) Write(p []byte) (n int, err error) {
	s := string(p)
	s = strings.TrimSpace(s)
	slog.Debug(s)
	return len(p), nil
}

// Println is called to log text
func (logger) Println(vs ...any) {
	s := fmt.Sprint(vs...)
	s = strings.TrimSpace(s)
	slog.Debug(s)
}

// Gphotos is a single page browser for Google Photos
type Gphotos struct {
	browser            *rod.Browser
	page               *rod.Page
	mu                 sync.Mutex // only one download at once is allowed
	completed          chan bool  // channel to signal download completion
	downloadInProgress chan bool  // download in progress
	downloadGUID       string     // GUID of the download
}

// New creates a new browser on the gphotos main page to check we are logged in
func New() (*Gphotos, error) {
	g := &Gphotos{}
	err := g.startBrowser()
	if err != nil {
		return nil, err
	}
	err = g.startServer()
	if err != nil {
		return nil, err
	}
	return g, nil
}

// start the browser off and check it is authenticated
func (g *Gphotos) startBrowser() error {
	// We use the default profile in our new data directory
	l := launcher.New().
		Bin(browserPath).
		Headless(!*show).
		UserDataDir(browserConfig).
		Preferences(browserPrefs).
		Set("disable-gpu").
		Set("disable-audio-output").
		Logger(logger{})

	url, err := l.Launch()
	if err != nil {
		return fmt.Errorf("browser launch: %w", err)
	}

	g.browser = rod.New().
		ControlURL(url).
		NoDefaultDevice().
		Trace(true).
		SlowMotion(100 * time.Millisecond).
		Logger(logger{})

	err = g.browser.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to browser: %w", err)
	}

	g.page, err = g.browser.Page(proto.TargetCreateTarget{URL: gphotosURL})
	if err != nil {
		return fmt.Errorf("couldn't open gphotos URL: %w", err)
	}

	eventCallback := func(e *proto.PageLifecycleEvent) {
		slog.Debug("Event", "Name", e.Name, "Dump", e)
	}
	g.page.EachEvent(eventCallback)

	err = g.page.WaitLoad()
	if err != nil {
		return fmt.Errorf("gphotos page load: %w", err)
	}

	authenticated := false
	for try := 0; try < 60; try++ {
		time.Sleep(1 * time.Second)
		info := g.page.MustInfo()
		slog.Debug("URL", "url", info.URL)
		// When not authenticated Google redirects away from the Photos URL
		if info.URL == gphotosURL {
			authenticated = true
			slog.Debug("Authenticated")
			break
		}
		slog.Info("Please log in, or re-run with -login flag")
	}
	if !authenticated {
		return errors.New("browser is not log logged in - rerun with the -login flag")
	}
	// Subscribe to BrowserDownloadWillBegin and BrowserDownloadProgress events
	g.downloadGUID = ""
	g.completed = make(chan bool, 1)
	g.downloadInProgress = make(chan bool, 1)

	slog.Debug("Subscribing to download events")
	go g.browser.EachEvent(func(e *proto.PageDownloadWillBegin) {
		g.downloadGUID = e.GUID
		slog.Debug("Download started", "guid", e.GUID)
	}, func(e *proto.PageDownloadProgress) {
		if e.GUID == g.downloadGUID {
			if e.State == proto.PageDownloadProgressStateCompleted {
				slog.Debug("Download completed", "guid", e.GUID)
				select {
				case g.downloadInProgress <- false: // Signal that download is no longer in progress
				default:
				}
				select {
				case g.completed <- true:
				default:
				}
			} else if e.State == proto.PageDownloadProgressStateInProgress {
				slog.Debug("Download in progress", "guid", e.GUID)
				select {
				case g.downloadInProgress <- true: // Signal that download is in progress
				default:
				}
			}
		}
	})()
	return nil
}

// start the web server off
func (g *Gphotos) startServer() error {
	http.HandleFunc("GET /", g.getRoot)
	http.HandleFunc("GET /id/{photoID}", g.getID)
	go func() {
		err := http.ListenAndServe(*addr, nil)
		if errors.Is(err, http.ErrServerClosed) {
			slog.Debug("web server closed")
		} else if err != nil {
			slog.Error("Error starting web server", "err", err)
			os.Exit(1)
		}
	}()
	return nil
}

// Serve the root page
func (g *Gphotos) getRoot(w http.ResponseWriter, r *http.Request) {
	slog.Info("got / request")
	_, _ = io.WriteString(w, `
<!DOCTYPE html>
<html lang="en">

<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>`+program+`</title>
  <link rel="stylesheet" href="styles.css">
</head>

<body>
  <h1>`+program+`</h1>
  <p>`+program+` is used to download full resolution Google Photos in combination with rclone.</p>
</body>

</html>`)
}

// Serve a photo ID
func (g *Gphotos) getID(w http.ResponseWriter, r *http.Request) {
	photoID := r.PathValue("photoID")
	slog.Info("got photo request", "id", photoID)
	path, err := g.Download(photoID)
	if err != nil {
		slog.Error("Download image failed", "id", photoID, "err", err)
		var h httpError
		if errors.As(err, &h) {
			w.WriteHeader(int(h))
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
		return
	}
	slog.Info("Downloaded photo", "id", photoID, "path", path)

	// Remove the file after it has been served
	defer func() {
		err := os.Remove(path)
		if err == nil {
			slog.Debug("Removed downloaded photo", "id", photoID, "path", path)
		} else {
			slog.Error("Failed to remove download directory", "id", photoID, "path", path, "err", err)
		}
	}()

	http.ServeFile(w, r, path)
}

// httpError wraps an HTTP status code
type httpError int

func (h httpError) Error() string {
	return fmt.Sprintf("HTTP Error %d", h)
}

// Download a photo with the ID given
//
// Returns the path to the photo which should be deleted after use
func (g *Gphotos) Download(photoID string) (string, error) {
	// Can only download one picture at once
	g.mu.Lock()
	defer g.mu.Unlock()
	url := gphotoURL + photoID

	slog := slog.With("id", photoID)

	// Create a new blank browser tab
	slog.Error("Open new tab")
	page, err := g.browser.Page(proto.TargetCreateTarget{})
	if err != nil {
		return "", fmt.Errorf("failed to open browser tab for photo %q: %w", photoID, err)
	}
	defer func() {
		err := page.Close()
		if err != nil {
			slog.Error("Error closing tab", "Error", err)
		}
	}()

	// Navigate to the photo URL
	slog.Debug("Navigate to photo URL")
	err = page.Navigate(url)
	if err != nil {
		return "", fmt.Errorf("failed to navigate to photo %q: %w", photoID, err)
	}

	// Wait for the page to reach the "NetworkAlmostIdle" state
	slog.Debug("Wait for page to reach NetworkAlmostIdle state")
	page.WaitNavigation(proto.PageLifecycleEventNameNetworkAlmostIdle)()

	// Drain the `completed` channel
	select {
	case <-g.completed:
		slog.Debug("Drained completed channel")
	default:
		// No value to drain
	}

	// Drain the `downloadInProgress` channel
	select {
	case <-g.downloadInProgress:
		slog.Debug("Drained downloadInProgress channel")
	default:
		// No value to drain
	}

	// Shift-D to download
	page.KeyActions().Press(input.ShiftLeft).Type('D').MustDo()

	// Wait for download
	slog.Debug("Wait for download")
	downloadGUID, err := g.waitForDownloadWithTimeout()
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	path := filepath.Join(downloadDir, downloadGUID)

	// Check file
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}

	slog.Debug("Download successful", "size", fi.Size(), "path", path)

	return path, nil
}

// waits for a download to complete or times out if no progress is detected
func (g *Gphotos) waitForDownloadWithTimeout() (string, error) {

	downloadTimeout := 10 * time.Second
	timer := time.NewTimer(downloadTimeout)
	defer timer.Stop()

	for {
		select {
		case <-g.completed:
			// Download completed successfully
			return g.downloadGUID, nil
		case inProgress := <-g.downloadInProgress:
			// Download in progress, wait for completion
			if inProgress {
				timer.Reset(downloadTimeout)
				slog.Debug("Timer reset due to download progress")
			}

		case <-timer.C:
			// Timeout reached
			err := fmt.Errorf("download stalled for more than %s (GUID: %s)", downloadTimeout, g.downloadGUID)
			g.downloadGUID = ""
			return "", err
		}
	}
}

// Close the browser
func (g *Gphotos) Close() {
	err := g.browser.Close()
	if err == nil {
		slog.Debug("Closed browser")
	} else {
		slog.Error("Failed to close browser", "err", err)
	}
}

func main() {
	err := config()
	if err != nil {
		slog.Error("Configuration failed", "err", err)
		os.Exit(2)
	}
	defer removeDownloadDirectory()

	// If login is required, run the browser standalone
	if *login {
		slog.Info("Log in to google with the browser that pops up, close it, then re-run this without the -login flag")
		cmd := exec.Command(browserPath, "--user-data-dir="+browserConfig, loginURL)
		err = cmd.Start()
		if err != nil {
			slog.Error("Failed to start browser", "err", err)
			os.Exit(2)
		}
		slog.Info("Waiting for browser to be closed")
		err = cmd.Wait()
		if err != nil {
			slog.Error("Browser run failed", "err", err)
			os.Exit(2)
		}
		slog.Info("Now restart this program without -login")
		os.Exit(1)
	}

	g, err := New()
	if err != nil {
		slog.Error("Failed to make browser", "err", err)
		os.Exit(2)
	}
	defer g.Close()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, exitSignals...)

	// Wait for CTRL-C or SIGTERM
	slog.Info("Press CTRL-C (or kill) to quit")
	sig := <-quit
	slog.Info("Signal received - shutting down", "signal", sig)
}
