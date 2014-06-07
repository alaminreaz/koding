package kloud

import (
	"fmt"
	"koding/db/mongodb"
	"koding/kites/kloud/digitalocean"
	"koding/kites/kloud/eventer"
	"koding/kites/kloud/idlock"
	"koding/kites/kloud/kloud/protocol"
	"koding/kodingkite"
	"koding/tools/config"
	"log"
	"os"

	"github.com/koding/kite"
	"github.com/koding/logging"
)

const (
	VERSION = "0.0.1"
	NAME    = "kloud"
)

var (
	providers = make(map[string]func() protocol.Provider)
)

type Kloud struct {
	Config *config.Config
	Log    logging.Logger
	Kite   *kite.Kite

	Storage  Storage
	Eventers map[string]eventer.Eventer

	idlock *idlock.IdLock

	Name    string
	Version string
	Region  string
	Port    int

	// needed for signing/generating kite tokens
	KontrolPublicKey  string
	KontrolPrivateKey string
	KontrolURL        string

	Debug bool
}

func (k *Kloud) NewKloud() *kodingkite.KodingKite {
	if k.Config == nil {
		panic("config is not initialized")
	}

	k.Name = NAME
	k.Version = VERSION

	k.idlock = idlock.New()

	if k.Log == nil {
		k.Log = createLogger(NAME, k.Debug)
	}

	if k.Storage == nil {
		k.Storage = &MongoDB{session: mongodb.NewMongoDB(k.Config.Mongo)}
	}

	if k.Eventers == nil {
		k.Eventers = make(map[string]eventer.Eventer)
	}

	kt, err := kodingkite.New(k.Config, k.Name, k.Version)
	if err != nil {
		log.Fatalln(err)
	}
	k.Kite = kt.Kite

	kt.Config.Region = k.Region
	kt.Config.Port = k.Port

	k.ControlFunc("build", k.build)
	k.ControlFunc("start", k.start)
	k.ControlFunc("stop", k.stop)
	k.ControlFunc("restart", k.restart)
	k.ControlFunc("destroy", k.destroy)
	k.ControlFunc("info", k.info)
	kt.HandleFunc("event", k.event)

	k.InitializeProviders()

	return kt
}

func (k *Kloud) SignFunc(username string) (string, string, error) {
	k.Log.Debug("Signing a key for user: '%s' kontrolURL: %s ", username, k.KontrolURL)
	return createKey(username, k.KontrolURL, k.KontrolPrivateKey, k.KontrolPublicKey)
}

func (k *Kloud) GetProvider(providerName string) (protocol.Provider, error) {
	providerFunc, ok := providers[providerName]
	if !ok {
		return nil, NewError(ErrProviderNotFound)
	}

	provider := providerFunc()
	return provider, nil
}

func (k *Kloud) InitializeProviders() {
	providers = map[string]func() protocol.Provider{
		"digitalocean": func() protocol.Provider {
			return &digitalocean.DigitalOcean{
				Log:      createLogger("digitalocean", k.Debug),
				SignFunc: k.SignFunc,
			}
		},
	}
}

func createLogger(name string, debug bool) logging.Logger {
	log := logging.NewLogger(name)
	logHandler := logging.NewWriterHandler(os.Stderr)
	logHandler.Colorize = true
	log.SetHandler(logHandler)

	if debug {
		fmt.Println("DEBUG mode is enabled.")
		log.SetLevel(logging.DEBUG)
		logHandler.SetLevel(logging.DEBUG)
	}

	return log
}
