// This code is under BSD license. See license-bsd.txt
package main

import (
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/garyburd/go-oauth/oauth"
	"github.com/gorilla/securecookie"
	"github.com/kjk/apptranslator/store"
	"github.com/kjk/u"
	netcontext "golang.org/x/net/context"
)

var (
	configPath = flag.String("config", "config.json", "Path to configuration file")
	httpAddr   = flag.String("addr", ":5001", "HTTP server address")
	//logPath      = flag.String("log", "stdout", "where to log")
	inProduction = flag.Bool("production", false, "are we running in production")
	noS3Backup   = flag.Bool("no-backup", false, "don't backup to s3")
	cookieName   = "ckie"
)

var (
	oauthClient = oauth.Client{
		TemporaryCredentialRequestURI: "https://api.twitter.com/oauth/request_token",
		ResourceOwnerAuthorizationURI: "https://api.twitter.com/oauth/authenticate",
		TokenRequestURI:               "https://api.twitter.com/oauth/access_token",
	}

	config = struct {
		TwitterOAuthCredentials *oauth.Credentials
		Apps                    []AppConfig
		CookieAuthKeyHexStr     *string
		CookieEncrKeyHexStr     *string
		AwsAccess               *string
		AwsSecret               *string
		S3BackupBucket          *string
		S3BackupDir             *string
	}{
		&oauthClient.Credentials,
		nil,
		nil, nil,
		nil, nil,
		nil, nil,
	}
	logger        *ServerLogger
	cookieAuthKey []byte
	cookieEncrKey []byte
	secureCookie  *securecookie.SecureCookie

	// this is where we store information about users and translation.
	// All in one place because I expect this data to be small
	dataDir string

	staticDir = "static"

	appState = AppState{}

	alwaysLogTime = true
)

func stringEmpty(s *string) bool {
	return s == nil || 0 == len(*s)
}

func s3BackupEnabled() bool {
	if *noS3Backup {
		logger.Notice("s3 backups disabled because -no-backup flag")
		return false
	}
	if !*inProduction {
		logger.Notice("s3 backups disabled because not in production")
		return false
	}
	if stringEmpty(config.AwsAccess) {
		logger.Notice("s3 backups disabled because AwsAccess not defined in config.json\n")
		return false
	}
	if stringEmpty(config.AwsSecret) {
		logger.Notice("s3 backups disabled because AwsSecret not defined in config.json\n")
		return false
	}
	if stringEmpty(config.S3BackupBucket) {
		logger.Notice("s3 backups disabled because S3BackupBucket not defined in config.json\n")
		return false
	}
	if stringEmpty(config.S3BackupDir) {
		logger.Notice("s3 backups disabled because S3BackupDir not defined in config.json\n")
		return false
	}
	return true
}

// data dir is ../../data on the server or ~/data/apptranslator locally
// the important part is that it's outside of directory with the code
func getDataDir() string {
	if dataDir != "" {
		return dataDir
	}

	dirsToCheck := []string{
		// on the server, must be done first because ExpandTildeInPath()
		// doesn't work when cross-compiled on mac for linux
		filepath.Join("..", "..", "data"),
		u.ExpandTildeInPath("~/data/apptranslator"),
	}

	for _, dir := range dirsToCheck {
		if u.PathExists(dir) {
			dataDir = dir
			return dataDir
		}
	}
	log.Fatalf("data directory (%v) doesn't exist\n", dirsToCheck)
	return ""
}

// AppConfig is a static configuration of a single app
type AppConfig struct {
	Name string
	// url for the application's website (shown in the UI)
	Url     string
	DataDir string
	// we authenticate only with Twitter, this is the twitter user name
	// of the admin user
	AdminTwitterUser  string
	AdminTwitterUser2 string
	// an arbitrary string, used to protect the API for uploading new strings
	// for the app
	UploadSecret string
}

// User describes an user
type User struct {
	Login string
}

// App describes an app
type App struct {
	AppConfig
	store *store.StoreCsv
}

// AppState describes state of the app
type AppState struct {
	Users []*User
	Apps  []*App
}

// NewApp creates new App
func NewApp(config *AppConfig) *App {
	app := &App{AppConfig: *config}
	return app
}

// LangsCount returns number of languages, used in templates
func (a *App) LangsCount() int {
	return len(store.Languages)
	//return a.store.LangsCount()
}

// StringsCount returns number of strings, used in templates
func (a *App) StringsCount() int {
	return a.store.StringsCount()
}

// UntranslatedCount returns number of untranslated strings, used in templates
func (a *App) UntranslatedCount() int {
	return a.store.UntranslatedCount()
}

// EditsCount returns number of edits
func (a *App) EditsCount() int {
	return a.store.EditsCount()
}

func (a *App) storeBinaryFilePath() string {
	// the data directory and file 'translations.dat' must already
	// exists. We don't expect adding new projects often, it requires a
	// deploy anyway, so we force the admin to create those dirs
	appDataDir := filepath.Join(getDataDir(), a.DataDir)
	dataFilePath := filepath.Join(appDataDir, "translations.dat")
	/*if !u.PathExists(dataFilePath) {
		log.Fatalf("Data file %s for app %s doesn't exist. Prease create (empty file is ok)!\n", dataFilePath, a.Name)
	}*/
	return dataFilePath
}

func (a *App) storeCsvFilePath() string {
	// the data directory and file 'translations.dat' must already
	// exists. We don't expect adding new projects often, it requires a
	// deploy anyway, so we force the admin to create those dirs
	appDataDir := filepath.Join(getDataDir(), a.DataDir)
	dataFilePath := filepath.Join(appDataDir, "translations.csv")
	/*if !u.PathExists(dataFilePath) {
		log.Fatalf("Data file %s for app %s doesn't exist. Prease create (empty file is ok)!\n", dataFilePath, a.Name)
	}*/
	return dataFilePath
}

func readAppData(app *App) error {
	var path string
	path = app.storeCsvFilePath()
	if u.PathExists(path) {
		if l, err := store.NewStoreCsv(path); err == nil {
			app.store = l
			return nil
		}
	}
	return fmt.Errorf("readAppData: %q data file doesn't exist", path)
}

func findApp(name string) *App {
	for _, app := range appState.Apps {
		if app.Name == name {
			return app
		}
	}
	return nil
}

func appAlreadyExists(name string) bool {
	return nil != findApp(name)
}

func appInvalidField(app *App) string {
	app.Name = strings.TrimSpace(app.Name)
	if app.Name == "" {
		return "Name"
	}
	if app.DataDir == "" {
		return "DataDir"
	}
	if app.AdminTwitterUser == "" {
		return "AdminTwitterUser"
	}
	if app.UploadSecret == "" {
		return "UploadSecret"
	}
	return ""
}

func addApp(app *App) error {
	if invalidField := appInvalidField(app); invalidField != "" {
		return fmt.Errorf("App has invalid field %q", invalidField)
	}
	if appAlreadyExists(app.Name) {
		return errors.New("App already exists")
	}
	if err := readAppData(app); err != nil {
		return err
	}
	appState.Apps = append(appState.Apps, app)
	return nil
}

func isTopLevelURL(url string) bool {
	return 0 == len(url) || "/" == url
}

func userIsAdmin(app *App, user string) bool {
	if user == "" {
		return false
	}
	return user == app.AdminTwitterUser || user == app.AdminTwitterUser2
}

// reads the configuration file from the path specified by
// the config command line flag.
func readConfig(configFile string) error {
	b, err := ioutil.ReadFile(configFile)
	if err != nil {
		return err
	}
	err = json.Unmarshal(b, &config)
	if err != nil {
		return err
	}
	cookieAuthKey, err = hex.DecodeString(*config.CookieAuthKeyHexStr)
	if err != nil {
		return err
	}
	cookieEncrKey, err = hex.DecodeString(*config.CookieEncrKeyHexStr)
	if err != nil {
		return err
	}
	secureCookie = securecookie.New(cookieAuthKey, cookieEncrKey)
	// verify auth/encr keys are correct
	val := map[string]string{
		"foo": "bar",
	}
	_, err = secureCookie.Encode(cookieName, val)
	if err != nil {
		// for convenience, if the auth/encr keys are not set,
		// generate valid, random value for them
		auth := securecookie.GenerateRandomKey(32)
		encr := securecookie.GenerateRandomKey(32)
		fmt.Printf("CookieAuthKeyHexStr and/or nCookieEncrKeyHexStr in config.json is not valid.\n")
		fmt.Printf("You can use those random values:\n")
		fmt.Printf("CookieAuthKeyHexStr: %s\nCookieEncrKeyHexStr: %s\n", hex.EncodeToString(auth), hex.EncodeToString(encr))
	}
	// TODO: somehow verify twitter creds
	return err
}

func makeTimingHandler(fn func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		startTime := time.Now()
		fn(w, r)
		duration := time.Now().Sub(startTime)
		// log urls that take long time to generate i.e. over 1 sec in production
		// or over 0.1 sec in dev
		shouldLog := duration.Seconds() > 1.0
		if alwaysLogTime && duration.Seconds() > 0.1 {
			shouldLog = true
		}
		if shouldLog {
			url := r.URL.Path
			if len(r.URL.RawQuery) > 0 {
				url = fmt.Sprintf("%s?%s", url, r.URL.RawQuery)
			}
			logger.Noticef("%q took %f seconds to serve", url, duration.Seconds())
		}
	}
}

func hostPolicy(ctx netcontext.Context, host string) error {
	if strings.HasSuffix(host, "apptranslator.org") {
		return nil
	}
	return errors.New("acme/autocert: only *.apptransloator.org hosts are allowed")
}

func main() {
	flag.Parse()

	if *inProduction {
		reloadTemplates = false
		alwaysLogTime = false
	}

	logger = NewServerLogger(256, 256, !*inProduction)

	/*
		if *logPath == "stdout" {
			logger = log.New(os.Stdout, "", 0)
		} else {
			loggerFile, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0666)
			if err != nil {
				log.Fatalf("Failed to open log file %q, %s\n", *logPath, err)
			}
			defer loggerFile.Close()
			logger = log.New(loggerFile, "", 0)
		}*/

	if err := readConfig(*configPath); err != nil {
		log.Fatalf("Failed reading config file %s. %s\n", *configPath, err)
	}

	for _, appData := range config.Apps {
		app := NewApp(&appData)
		if err := addApp(app); err != nil {
			log.Fatalf("Failed to add the app: %s, err: %s\n", app.Name, err)
		} else {
			logger.Noticef("Added app %s\n", app.Name)
		}
	}

	// for testing, add a dummy app if no apps exist
	if len(appState.Apps) == 0 {
		log.Fatalf("No apps defined in config.json")
	}

	backupConfig := &BackupConfig{
		AwsAccess: *config.AwsAccess,
		AwsSecret: *config.AwsSecret,
		Bucket:    *config.S3BackupBucket,
		S3Dir:     *config.S3BackupDir,
		LocalDir:  getDataDir(),
	}

	if s3BackupEnabled() {
		go s3BackupLoop(backupConfig)
	}

	if *inProduction {
		m := autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: hostPolicy,
		}
		srv := makeHTTPServer()
		srv.Addr = ":443"
		srv.TLSConfig = &tls.Config{GetCertificate: m.GetCertificate}
		logger.Noticef("Started runing HTTPS on %s\n", srv.Addr)
		go func() {
			srv.ListenAndServeTLS("", "")
		}()
	}

	srv := makeHTTPServer()
	srv.Addr = *httpAddr
	logger.Noticef("Started running on %s. Data dir: %s\n", srv.Addr, getDataDir())
	if err := srv.ListenAndServe(); err != nil {
		fmt.Printf("http.ListendAndServer() failed with %q\n", err)
	}
	fmt.Printf("Exited\n")
}
