package bwhatsapp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
)

var (
	errFetchWAVersion = errors.New("fetching WhatsApp web version")
	errNoWAVersion    = errors.New("no version returned")

	// fetchLatestWAVersion is whatsmeow's scrape of web.whatsapp.com; a var so
	// tests never reach the network.
	fetchLatestWAVersion = whatsmeow.GetLatestVersion

	waVersionFetchTimeout = 10 * time.Second

	// waVersionMu orders our writes to whatsmeow's process-wide version.
	waVersionMu sync.Mutex
)

// refreshWAVersion makes whatsmeow use the WhatsApp web client version that
// web.whatsapp.com currently serves, when it is newer than the one in use.
// WhatsApp rejects an outdated version with a 405 at connect. It returns the
// version in use afterwards; on a failed fetch that is the unchanged one.
func refreshWAVersion(ctx context.Context) (store.WAVersionContainer, error) {
	waVersionMu.Lock()
	defer waVersionMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, waVersionFetchTimeout)
	defer cancel()

	latest, err := fetchLatestWAVersion(ctx, nil)
	if err == nil && latest == nil {
		err = errNoWAVersion
	}

	if err != nil {
		// whatsmeow puts the whole page body into a non-200 error.
		return store.GetWAVersion(), fmt.Errorf("%w: %.200s", errFetchWAVersion, err.Error())
	}

	if store.GetWAVersion().LessThan(*latest) {
		store.SetWAVersion(*latest)
	}

	return store.GetWAVersion(), nil
}

// useLatestWAVersion refreshes the client version before a connect and logs
// the one in use.
func (b *Bwhatsapp) useLatestWAVersion() {
	version, err := refreshWAVersion(context.Background())
	if err != nil {
		b.Log.Warnf("Could not refresh the WhatsApp web version: %v", err)
	}

	b.Log.Infof("Using WhatsApp web version %s", version)
}
